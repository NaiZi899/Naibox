package backup

// s3.go 直连对象存储（AWS S3 协议，path-style）。
//
// # 为什么自己写 SigV4 而不是引 AWS SDK
//
// 本仓库的依赖表只有 10 项，且发行包体积是硬指标（`internal/desktop` 里
// 每加一份静态资源都要算）。S3 备份只需要四个动作：PUT / GET / LIST / HEAD，
// SigV4 的算法本身是固定的 HMAC 链，标准库足够。引 SDK 会带进 minio-go 或
// aws-sdk-go-v2 的整棵依赖树（含 credentials 链、ec2 metadata 探测），
// 那部分代码在本场景里一行都用不上。
//
// # 为什么用 path-style + UNSIGNED-PAYLOAD
//
// path-style（`/bucket/key`）是 MinIO、R2、华为 OBS、七牛这些兼容实现都认的
// 形态；virtual-hosted-style 需要通配证书，配置错时的报错极难懂。
//
// `UNSIGNED-PAYLOAD` 让上传不必先算一遍整包 sha256（多读一遍几十 MB 的文件），
// 代价是依靠 HTTPS 传输完整性。HTTPS 端点：可以接受；HTTP 端点：装配期
// 直接拒绝（见 NewS3Channel），否则中间人可以改包而我们无从发现。

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3Options 一条 S3 通道的配置（来自 config.json）。
type S3Options struct {
	Endpoint  string // https://s3.example.com（可带路径前缀）
	Bucket    string
	Region    string // 空 = us-east-1
	Prefix    string // 对象名前缀（可空）
	AccessKey string
	SecretKey string
}

// S3Channel 对象存储通道。
type S3Channel struct {
	id   string
	opt  S3Options
	hc   *http.Client
	now  func() time.Time
	keep int
}

// NewS3Channel 构造。
//
// 端点必须是 https（见文件头）；本地 MinIO 常用 http，此时应当用
// `kind=local` 或前面挂一层反代 —— 这条纪律写在装配期，不留给调用方猜。
func NewS3Channel(id string, opt S3Options) (*S3Channel, error) {
	if strings.TrimSpace(opt.Endpoint) == "" {
		return nil, errors.New("s3: endpoint 为空")
	}
	if strings.TrimSpace(opt.Bucket) == "" {
		return nil, errors.New("s3: bucket 为空")
	}
	if opt.AccessKey == "" || opt.SecretKey == "" {
		return nil, errors.New("s3: access_key / secret_key 为空")
	}
	u, err := url.Parse(opt.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("s3: endpoint 解析失败: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("s3: endpoint 必须是 https（当前 %q）—— 包内含账号凭据，明文链路不可接受", u.Scheme)
	}
	if opt.Region == "" {
		opt.Region = "us-east-1"
	}
	opt.Endpoint = strings.TrimRight(opt.Endpoint, "/")
	opt.Prefix = strings.Trim(opt.Prefix, "/")
	return &S3Channel{
		id:   id,
		opt:  opt,
		hc:   &http.Client{Timeout: 15 * time.Minute},
		now:  time.Now,
		keep: 10,
	}, nil
}

func (c *S3Channel) ID() string   { return c.id }
func (c *S3Channel) Kind() string { return "s3" }
func (c *S3Channel) SetKeep(n int) {
	if n > 0 {
		c.keep = n
	}
}

// objectKey 拼接对象名（prefix + 包名）。
func (c *S3Channel) objectKey(name string) string {
	if c.opt.Prefix == "" {
		return name
	}
	return c.opt.Prefix + "/" + name
}

// Test HEAD 桶。200/204 = 通；403 = 凭据或权限不对；404 = 桶不存在。
func (c *S3Channel) Test(ctx context.Context) error {
	req, err := c.signedRequest(ctx, http.MethodHead, "", nil, 0)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接失败: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == http.StatusForbidden:
		return errors.New("权限不足（403）：检查 AccessKey/SecretKey 与该桶的写权限")
	case resp.StatusCode == http.StatusNotFound:
		return errors.New("桶不存在（404）：检查 bucket 名与 endpoint")
	default:
		return fmt.Errorf("端点返回 %s", resp.Status)
	}
}

// Put 上传一个包。
func (c *S3Channel) Put(ctx context.Context, name string, r io.Reader, size int64) (string, error) {
	key := c.objectKey(name)
	req, err := c.signedRequest(ctx, http.MethodPut, key, r, size)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/zip")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("上传被拒绝（%s）: %s", resp.Status, s3ErrText(body))
	}
	return fmt.Sprintf("%s/%s/%s", c.opt.Endpoint, c.opt.Bucket, key), nil
}

// List 列 prefix 下的对象（新的在前）。
func (c *S3Channel) List(ctx context.Context) ([]Object, error) {
	var out []Object
	token := ""
	for page := 0; page < 5; page++ {
		q := map[string]string{"list-type": "2"}
		if c.opt.Prefix != "" {
			q["prefix"] = c.opt.Prefix + "/"
		}
		if token != "" {
			q["continuation-token"] = token
		}
		req, err := c.signedRequestQuery(ctx, http.MethodGet, "", nil, 0, q)
		if err != nil {
			return nil, err
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("列举失败: %w", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("列举被拒绝（%s）: %s", resp.Status, s3ErrText(raw))
		}
		var doc struct {
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
			Contents              []struct {
				Key          string `xml:"Key"`
				Size         int64  `xml:"Size"`
				LastModified string `xml:"LastModified"`
			} `xml:"Contents"`
		}
		if err := xml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("列举响应无法解析: %w", err)
		}
		for _, it := range doc.Contents {
			base := it.Key
			if i := strings.LastIndex(base, "/"); i >= 0 {
				base = base[i+1:]
			}
			if !strings.HasPrefix(base, localPrefix) || !strings.HasSuffix(base, localSuffix) {
				continue
			}
			t, _ := time.Parse(time.RFC3339, it.LastModified)
			out = append(out, Object{Name: base, Size: it.Size, ModTime: t})
		}
		if !doc.IsTruncated || doc.NextContinuationToken == "" {
			break
		}
		token = doc.NextContinuationToken
	}
	sortNewestFirst(out)
	return out, nil
}

// Get 取回一个包。
func (c *S3Channel) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if !isBackupName(name) {
		return nil, errors.New("不是备份包文件名")
	}
	req, err := c.signedRequest(ctx, http.MethodGet, c.objectKey(name), nil, 0)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, fmt.Errorf("下载被拒绝（%s）: %s", resp.Status, s3ErrText(raw))
	}
	return resp.Body, nil
}

// s3ErrText 从 S3 的 XML 错误体里摘出 Code/Message。
//
// 原样下发整段 XML 对用户没有帮助（多半是 AccessDenied 的机翻），
// 而"没有这段"又会让 403 变成一句无信息的状态码。取两条就够定位。
func s3ErrText(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return "(空响应体)"
	}
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if err := xml.Unmarshal(body, &e); err == nil && (e.Code != "" || e.Message != "") {
		return strings.TrimSpace(e.Code + " " + e.Message)
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func (c *S3Channel) signedRequest(ctx context.Context, method, key string, body io.Reader, size int64) (*http.Request, error) {
	return c.signedRequestQuery(ctx, method, key, body, size, nil)
}

// signedRequestQuery 构造并签名一个请求。
func (c *S3Channel) signedRequestQuery(ctx context.Context, method, key string, body io.Reader, size int64, query map[string]string) (*http.Request, error) {
	target := c.opt.Endpoint + "/" + c.opt.Bucket
	canonicalURI := "/" + c.opt.Bucket
	if key != "" {
		canonicalURI += "/" + escapePath(key)
		target += "/" + escapePath(key)
	}
	canonicalQuery, rawQuery := canonicalQueryOf(query)
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 && body != nil {
		req.ContentLength = size
	}
	amzDate := c.now().UTC().Format("20060102T150405Z")
	dateShort := amzDate[:8]
	const payloadHash = "UNSIGNED-PAYLOAD"

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	host := req.URL.Host
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := method + "\n" + canonicalURI + "\n" + canonicalQuery + "\n" +
		canonicalHeaders + "\n" + signedHeaders + "\n" + payloadHash
	scope := dateShort + "/" + c.opt.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	key0 := "AWS4" + c.opt.SecretKey
	k := hmacSHA256([]byte(key0), dateShort)
	k = hmacSHA256(k, c.opt.Region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.opt.AccessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+sig)
	return req, nil
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalQueryOf 返回 (canonicalQuery, rawQuery)。
//
// SigV4 要求按 key 排序并对 key/value 做 RFC3986 编码；rawQuery 是真正放进
// URL 的那份 —— 两者必须同源，否则"签的"与"发的"不一致，服务端会回
// SignatureDoesNotMatch（而这个错误不会告诉你差在哪里）。
func canonicalQueryOf(query map[string]string) (string, string) {
	if len(query) == 0 {
		return "", ""
	}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, escapeQuery(k)+"="+escapeQuery(query[k]))
	}
	cq := strings.Join(parts, "&")
	return cq, cq
}

// escapePath 按 RFC3986 编码路径，**保留** '/'。
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = escapeQuery(s)
	}
	return strings.Join(segs, "/")
}

// escapeQuery 按 RFC3986 编码（空格是 %20，不是 '+'）。
func escapeQuery(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}
