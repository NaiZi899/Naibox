package boot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newBackupTestStore 造一个最小可用的 backupStore：dataRoot 里放 config.json。
func newBackupTestStore(t *testing.T, cfg *Config) (*backupStore, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, []byte("{\n  \"listen\": \":7863\"\n}\n"), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	if cfg == nil {
		cfg = &Config{}
	}
	if err := cfg.normalizeBackup(); err != nil {
		t.Fatalf("normalizeBackup: %v", err)
	}
	s, err := newBackupStore(path, root, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newBackupStore: %v", err)
	}
	return s, path
}

// TestBackupStoreAddsImplicitLocalChannel 老配置没有 backup 段时，本地通道必须
// 仍然存在 —— 否则托盘那个「备份数据」按钮会从"打一个 zip"退化成"报错"。
func TestBackupStoreAddsImplicitLocalChannel(t *testing.T) {
	s, _ := newBackupTestStore(t, nil)
	chs := s.Manager().Channels()
	if len(chs) != 1 || chs[0].Kind() != "local" {
		t.Fatalf("期望隐式 local 通道，实际 %d 条", len(chs))
	}
	// 真跑一次：包必须落进 <dataRoot>/backups/
	if _, err := s.Manager().Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(s.dataRoot, "backups"))
	if err != nil {
		t.Fatalf("读备份目录: %v", err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".zip") {
		t.Fatalf("期望 1 个 zip，实际 %v", entries)
	}
}

// TestBackupSaveKeepsSecrets 空串凭据 = 沿用旧值。
//
// 这是本功能最危险的一处：管理台 GET 到的是掩码（…abcd）。如果 PUT 把掩码
// 当新密钥写回，用户的真实密钥就被覆盖成一个不可用的字符串，而且**不可逆**。
// 所以约定是"空串 = 沿用"，这条测试钉的就是它。
func TestBackupSaveKeepsSecrets(t *testing.T) {
	cfg := &Config{}
	cfg.Backup.Channels = []BackupChannelConfig{{
		ID: "s3main", Kind: "s3", Endpoint: "https://s3.example.com", Bucket: "b",
		AccessKey: "AKIDOLD", SecretKey: "SECRETOLD", Prefix: "naibox",
	}}
	s, cfgPath := newBackupTestStore(t, cfg)

	patch := `{"enabled":false,"hours":[3],"keep":5,
	  "channels":[{"id":"s3main","kind":"s3","endpoint":"https://s3.example.com",
	               "bucket":"b","prefix":"naibox","access_key":"","secret_key":""}]}`
	if _, _, err := s.Save(json.RawMessage(patch)); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("读配置: %v", err)
	}
	var doc struct {
		Backup struct {
			Keep     int `json:"keep"`
			Hours    []int
			Channels []struct {
				ID        string `json:"id"`
				AccessKey string `json:"access_key"`
				SecretKey string `json:"secret_key"`
			} `json:"channels"`
		} `json:"backup"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("配置不是合法 JSON: %v", err)
	}
	if doc.Backup.Keep != 5 {
		t.Fatalf("keep 未写入: %d", doc.Backup.Keep)
	}
	if len(doc.Backup.Channels) != 1 {
		t.Fatalf("通道数不符: %d", len(doc.Backup.Channels))
	}
	if got := doc.Backup.Channels[0].AccessKey; got != "AKIDOLD" {
		t.Fatalf("空串凭据应当沿用旧值，实际写成 %q", got)
	}
	if got := doc.Backup.Channels[0].SecretKey; got != "SECRETOLD" {
		t.Fatalf("空串 secret 应当沿用旧值，实际写成 %q", got)
	}
	// 保存后必须热生效：通道表重建
	if n := len(s.Manager().Channels()); n != 1 {
		t.Fatalf("保存后通道表应重建（1 条），实际 %d", n)
	}
}

// TestBackupSaveRejectsBadConfig 坏配置必须在写盘前就被拒（否则下一次启动起不来）。
func TestBackupSaveRejectsBadConfig(t *testing.T) {
	s, cfgPath := newBackupTestStore(t, nil)
	before, _ := os.ReadFile(cfgPath)
	cases := []string{
		// s3 的 http 端点：包内含凭据，明文链路不可接受
		`{"channels":[{"id":"s3","kind":"s3","endpoint":"http://minio:9000","bucket":"b","access_key":"a","secret_key":"s"}]}`,
		// 重复 id
		`{"channels":[{"id":"x","kind":"local"},{"id":"x","kind":"local"}]}`,
		// 未知 kind
		`{"channels":[{"id":"x","kind":"ftp"}]}`,
		// 非法整点
		`{"hours":[24],"channels":[{"id":"x","kind":"local"}]}`,
		// 开了定时却没有通道
		`{"enabled":true,"hours":[3]}`,
	}
	for _, c := range cases {
		if _, _, err := s.Save(json.RawMessage(c)); err == nil {
			t.Fatalf("应当拒绝: %s", c)
		}
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Fatal("校验失败时不得写盘（配置被改坏了就起不来）")
	}
}

// TestBackupMaskTail 短密钥整串掩掉，长密钥只留尾 4 位。
func TestBackupMaskTail(t *testing.T) {
	if got := maskTail(""); got != "" {
		t.Fatalf("空串应当原样返回空，实际 %q", got)
	}
	if got := maskTail("abcd"); got != "****" {
		t.Fatalf("不足 8 位应整串掩掉，实际 %q", got)
	}
	if got := maskTail("AKIDEXAMPLE1234"); !strings.HasSuffix(got, "1234") || strings.Contains(got, "AKID") {
		t.Fatalf("只该留尾 4 位，实际 %q", got)
	}
}

// TestBackupTickOncePerHour 同一个整点只跑一次（5 分钟一跳的 ticker 会命中 12 次）。
func TestBackupTickOncePerHour(t *testing.T) {
	cfg := &Config{}
	cfg.Backup.Enabled = true
	cfg.Backup.Hours = []int{3}
	cfg.Backup.Channels = []BackupChannelConfig{{ID: "local", Kind: "local"}}
	s, _ := newBackupTestStore(t, cfg)

	at := time.Date(2026, 10, 24, 3, 5, 0, 0, time.Local)
	ctx := context.Background()
	s.tick(ctx, at)
	s.tick(ctx, at.Add(5*time.Minute))
	s.tick(ctx, at.Add(10*time.Minute))

	entries, err := os.ReadDir(filepath.Join(s.dataRoot, "backups"))
	if err != nil {
		t.Fatalf("读备份目录: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("同一个整点只该备份一次，实际 %d 个包", len(entries))
	}

	// 非配置整点不跑
	s.tick(ctx, at.Add(2*time.Hour))
	entries, _ = os.ReadDir(filepath.Join(s.dataRoot, "backups"))
	if len(entries) != 1 {
		t.Fatalf("非配置整点不该备份，实际 %d 个包", len(entries))
	}
}

// TestBackupTickHonoursEnabled 定时关闭时 tick 什么都不做。
func TestBackupTickHonoursEnabled(t *testing.T) {
	cfg := &Config{}
	cfg.Backup.Hours = []int{3}
	cfg.Backup.Channels = []BackupChannelConfig{{ID: "local", Kind: "local"}}
	s, _ := newBackupTestStore(t, cfg)
	s.tick(context.Background(), time.Date(2026, 10, 24, 3, 1, 0, 0, time.Local))
	entries, _ := os.ReadDir(filepath.Join(s.dataRoot, "backups"))
	if len(entries) != 0 {
		t.Fatalf("enabled=false 时不该备份，实际 %d 个包", len(entries))
	}
}

// TestBackupSnapshotMasksCredentials 快照里的凭据必须是掩码。
func TestBackupSnapshotMasksCredentials(t *testing.T) {
	cfg := &Config{}
	cfg.Backup.Channels = []BackupChannelConfig{{
		ID: "s3", Kind: "s3", Endpoint: "https://s3.example.com", Bucket: "b",
		AccessKey: "AKIDEXAMPLE", SecretKey: "VERYSECRETVALUE",
	}}
	s, _ := newBackupTestStore(t, cfg)
	v := s.Snapshot(context.Background())
	if len(v.Channels) != 1 {
		t.Fatalf("通道数不符: %d", len(v.Channels))
	}
	ch := v.Channels[0]
	for label, got := range map[string]string{"access": ch.AccessKey, "secret": ch.SecretKey} {
		if strings.Contains(got, "EXAMPLE") || strings.Contains(got, "SECRETVALUE") {
			t.Fatalf("%s 凭据没有被掩码: %q", label, got)
		}
	}
}
