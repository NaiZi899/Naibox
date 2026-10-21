// customstore.go 管理台「自定义上游」页的宿主实现。
//
// # 为什么放在 boot 而不是 admin
//
// 它要读写的是一段 **config.json**，而"配置文件的格式、默认值、原子写"
// 都是宿主的事实（`settings_store.go` 是同一个理由）。admin 只定义接口，
// 于是它仍然可以脱离配置文件被单测。
//
// # 校验只有一份
//
// 启用的条目一律过 `custom.NormalizeAll` —— 与启动期完全同一份判据。
// 保存时校验宽于启动期会让"管理页保存成功、下次启动失败"，那是最难查的一类。
package boot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/NaiZi899/Naibox/internal/admin"
	"github.com/NaiZi899/Naibox/internal/custom"
)

type customStore struct {
	path string
	cfg  *Config
	// mu 串行化管理页的读改写。
	//
	// 两个并发的 PUT 在同一份 config.json 上做"读原文件 → 合并 → 原子写"会
	// 互相覆盖（后写的胜出，先写的那次从界面上看是"保存成功"）。另外
	// List 会读 cfg.CustomUpstreams，而 Replace 会替换它 —— 没有这把锁，
	// 读到一个正在被替换的切片是数据竞争。
	mu sync.Mutex
}

// newCustomStore 构造管理页的读写实现。
func newCustomStore(path string, cfg *Config) admin.CustomUpstreamStore {
	return &customStore{path: path, cfg: cfg}
}

// ListCustomUpstreams 列出配置里的全部条目（含 disabled）。
//
// 刻意**不做网络探测**：列表会在每次进面板时加载，N 个端点各打一次上游
// 会让"打开管理页"变成一次批量请求。探测由每个端点自己的面板与
// 「测试连通性」按钮触发。
func (s *customStore) ListCustomUpstreams() ([]admin.CustomUpstreamView, error) {
	s.mu.Lock()
	specs := append([]custom.Spec(nil), s.cfg.CustomUpstreams...)
	s.mu.Unlock()

	out := make([]admin.CustomUpstreamView, 0, len(specs))
	for _, spec := range specs {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			name = spec.ID
		}
		shards := spec.Shards
		if shards <= 0 {
			shards = 1
		}
		view := admin.CustomUpstreamView{
			ID:      spec.ID,
			Name:    name,
			BaseURL: spec.BaseURL,
			Enabled: spec.IsEnabled(),
			Shards:  shards,
		}
		// 可编辑字段全量回显：PUT 是整表覆盖，少给一个字段就会被一次保存抹掉。
		view.APIKeyEnv = spec.APIKeyEnv
		view.Models = append([]string(nil), spec.Models...)
		view.ChatPath = spec.ChatPath
		view.ModelsPath = spec.ModelsPath
		view.AuthHeader = spec.AuthHeader
		view.AuthScheme = spec.AuthScheme
		view.Headers = spec.Headers
		view.TimeoutSeconds = spec.TimeoutSeconds
		view.InsecureSkipVerify = spec.InsecureSkipVerify
		// 密钥只回显尾 4 位；env 形态下读当前环境（读不到就留空）。
		key := strings.TrimSpace(spec.APIKey)
		if key == "" && strings.TrimSpace(spec.APIKeyEnv) != "" {
			key = strings.TrimSpace(os.Getenv(strings.TrimSpace(spec.APIKeyEnv)))
		}
		view.KeyTail = custom.MaskKey(key)

		// 用与启动期同一份 Normalize 补齐模型数与默认值；失败只影响展示，
		// 不阻断列表（否则一条写错的 disabled 条目会让整页打不开）。
		if ep, err := custom.Normalize(spec, os.Getenv); err == nil {
			view.Name = ep.Name
			view.BaseURL = ep.BaseURL
			view.Shards = ep.Shards
			view.ModelsCount = len(ep.StaticModels)
			view.TimeoutSeconds = int(ep.Timeout.Seconds())
			if view.Models == nil {
				view.Models = append([]string(nil), ep.StaticModels...)
			}
		} else if view.Enabled {
			view.Probe.Error = err.Error()
		}
		out = append(out, view)
	}
	return out, nil
}

// ReplaceCustomUpstreams 整体替换配置段。
//
// 只改 `custom_upstreams` 这一个键，其余顶层键原样保留（用户手写的注释键、
// 其它段的未知字段都不该因为一次保存消失）。写入用临时文件 + rename，
// 避免半截 JSON 让下次启动失败。
func (s *customStore) ReplaceCustomUpstreams(items json.RawMessage) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var specs []custom.Spec
	if err := json.Unmarshal(items, &specs); err != nil {
		return nil, fmt.Errorf("items 不是合法的配置数组: %w", err)
	}
	// 密钥沿用：列表接口刻意不回显明文，于是"改个名字"提交上来的 api_key 是空的。
	// 这里按 id 从旧配置继承密钥，避免一次编辑把 key 抹掉（那会让端点直接 401）。
	specs = inheritKeys(specs, s.cfg.CustomUpstreams)

	// 启用条目必须能通过启动期校验；停用条目只随原样落盘。
	eps, err := custom.NormalizeAll(specs, os.Getenv)
	if err != nil {
		return nil, err
	}
	// 落盘的是**合并后的 specs**（不是原始 items）：否则继承来的密钥只存在于
	// 内存里，重启后又变回空 key。未知字段会被丢掉，这是已知取舍 ——
	// 保住密钥比保住未声明的字段重要。
	merged, err := json.Marshal(specs)
	if err != nil {
		return nil, err
	}
	if err := writeConfigKey(s.path, "custom_upstreams", merged); err != nil {
		return nil, err
	}
	s.cfg.CustomUpstreams = specs
	s.cfg.CustomEndpoints = eps
	return []string{"custom_upstreams"}, nil
}

// inheritKeys 给"提交时没带密钥"的条目补上旧配置里的 api_key / api_key_env。
//
// 判据是 **id 相同且两个密钥字段都为空**：
//   - 只空一个 → 用户显式在切换形态（key ↔ env），尊重提交值，不继承；
//   - 两个都空 → 列表页没有回显密钥，只可能是"没改密钥"，继承。
//
// 新建条目（id 在旧配置里找不到）两个都空时保持空，交给 NormalizeAll 报错。
func inheritKeys(in, old []custom.Spec) []custom.Spec {
	oldByID := make(map[string]custom.Spec, len(old))
	for _, o := range old {
		oldByID[o.ID] = o
	}
	out := make([]custom.Spec, len(in))
	copy(out, in)
	for i := range out {
		if out[i].ClearAPIKey {
			// 显式清空：不要把旧 key 复活。清空后仍要过 NormalizeAll ——
			// 启用中的条目会因此报"必须填一个"，那正是"先停用再清空"的用法。
			out[i].APIKey = ""
			out[i].APIKeyEnv = ""
			out[i].ClearAPIKey = false // 语义位只在本次请求里有效，不落盘
			continue
		}
		if strings.TrimSpace(out[i].APIKey) != "" || strings.TrimSpace(out[i].APIKeyEnv) != "" {
			continue
		}
		if prev, ok := oldByID[out[i].ID]; ok {
			out[i].APIKey = prev.APIKey
			out[i].APIKeyEnv = prev.APIKeyEnv
		}
	}
	return out
}

// TestCustomUpstream 探测一条草稿（不落盘）。
func (s *customStore) TestCustomUpstream(spec json.RawMessage) (admin.CustomProbeView, error) {
	var one custom.Spec
	if err := json.Unmarshal(spec, &one); err != nil {
		return admin.CustomProbeView{}, fmt.Errorf("草稿不是合法的配置对象: %w", err)
	}
	// 行内「测试」拿到的是列表投影（没有明文密钥）：同样按 id 继承，
	// 否则它会永远返回"配置非法"，而端点其实是好的。
	// 只把旧配置**拷一份**再出锁：探测最长 5 秒，不能拿着锁等网络。
	s.mu.Lock()
	old := append([]custom.Spec(nil), s.cfg.CustomUpstreams...)
	s.mu.Unlock()
	inherited := inheritKeys([]custom.Spec{one}, old)
	res := custom.ProbeSpec(context.Background(), inherited[0], os.Getenv)
	return admin.CustomProbeView{
		OK:          res.OK,
		Status:      res.Status,
		LatencyMS:   res.LatencyMS,
		ModelsCount: res.ModelsCount,
		Error:       res.Error,
		At:          res.At.Format("2006-01-02T15:04:05Z07:00"),
	}, nil
}

// writeConfigKey 读原 JSON → 替换一个顶层键 → 原子写回。
//
// 与 settings_store.go 的 persist 同一条纪律：**不要**把 Config 结构体整体
// 序列化回去 —— 那会丢掉用户手写的未知键与字段顺序，一次保存就等于重写配置。
func writeConfigKey(path, key string, value json.RawMessage) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("配置不是合法 JSON: %w", err)
	}
	doc[key] = value

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	mode := os.FileMode(0o600)
	if st, statErr := os.Stat(path); statErr == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里删不到东西，是幂等的兜底
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	return nil
}
