// models_supports_images_test.go —— /v1/models 的**视觉能力声明**回归。
//
// # 这次修的是什么
//
// 上游一直如实上报"这个模型收不收图片"，字段名各不相同：
//
//	workbuddy  supportsImages
//	trae       display_config.multimodal
//	loomy      capabilities.vision
//	inkstone   input_modalities[].type 含 "image"
//	jiyuan     supports_vision
//
// 而网关把它们**全部丢掉**，`/v1/models` 里没有任何一条记录说得出
// "这个模型能收图"。后果在调用端：DSH 的 llm-pi-ai 对没有模态声明的模型
// 回落 `defaultInput = ["text"]`，于是**所有**视觉模型在界面上都显示
// "不支持图片"，哪怕上游明写支持。
//
// # 为什么用**非默认上游**做载体
//
// /v1/models 有两条互不相通的取数路径：
//
//	默认上游      singleProviderModelList → fetchDynamicModels → cfg.Upstream
//	              （workbuddy 的客户端，测试里指向不可达地址 → 回落静态表）
//	其余上游      ProviderRouter.Models(id)
//
// 本桩的默认上游是 loomy，所以把测试模型挂在 loomy 上**一条都看不到**。
// 挂在非默认的 inkstone 上才走 router 那条路径。
//
// # 本文件钉住四条判据
//
//  1. 声明必须真的透传（上游 true → 目录里有 supports_images）
//  2. 未声明**不得**猜（上游没表态 → 目录里没有这个键，而不是 false）
//  3. 同一个模型既是对话模型又是生图模型时**只出现一条**，两边信息都保留
//  4. 只在生图声明里的模型照旧进目录（合并没把它吞掉）
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// nonDefaultProvider 本文件统一的载体：**不是**桩里的默认上游（loomy）。
const nonDefaultProvider = "inkstone"

// nonDefaultRouter 造一个"默认上游 + 一个非默认上游"的路由桩。
//
// ⚠ `reg` 是内嵌 multiRouter 的提升字段，**不能在复合字面量里初始化**
// （go vet: "use of promoted field … requires go1.27 or later"，而本模块
// 的 go 指令低于它）。所以先构造再赋值。
func nonDefaultRouter(mut func(*imageRouter)) *imageRouter {
	r := &imageRouter{ids: []string{"loomy", nonDefaultProvider}}
	r.reg = map[string]*recordingProvider{
		"loomy":            {id: "loomy"},
		nonDefaultProvider: {id: nonDefaultProvider},
	}
	if mut != nil {
		mut(r)
	}
	return r
}

// catalogOf 拉一次 /v1/models 并按 id 归组。
//
// 返回 map[id][]entry 而不是 map[id]entry —— **重复条目正是本文件要抓的
// 缺陷**，用后写覆盖前写的 map 会让它消失。
func catalogOf(t *testing.T, r ProviderRouter) map[string][]map[string]any {
	t.Helper()
	h := newImagesTestHandler(t, r)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models 状态码 = %d；body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法的模型列表: %v；body=%s", err, w.Body.String())
	}
	out := make(map[string][]map[string]any, len(body.Data))
	for _, e := range body.Data {
		id, _ := e["id"].(string)
		out[id] = append(out[id], e)
	}
	return out
}

// single 取某 id 的唯一一条记录；条数不对就直接判失败。
func single(t *testing.T, cat map[string][]map[string]any, id string) map[string]any {
	t.Helper()
	entries, ok := cat[id]
	if !ok {
		t.Fatalf("目录里没有 %s", id)
	}
	if len(entries) != 1 {
		t.Fatalf("%s 出现 %d 条，应当恰好 1 条", id, len(entries))
	}
	return entries[0]
}

// TestSupportsImagesFlowsIntoCatalog 上游声明了就必须出现在目录里。
//
// 两个模型**都要放**：只验正例的话，"把 SupportsImages 硬编码成 true"
// 也能过 —— 反例才钉得住"这个值确实来自上游"。
//
// 取值照抄真实上游（inkstone 的 kimi-k2.6 上游报 image、glm-5.3 只报 text）。
func TestSupportsImagesFlowsIntoCatalog(t *testing.T) {
	r := nonDefaultRouter(func(r *imageRouter) {
		r.modelsByProvider = map[string][]gateway.ModelInfo{
			nonDefaultProvider: {
				{ID: "kimi-k2.6", ContextWindow: 262144, MaxOutputTokens: 8192, SupportsImages: true},
				{ID: "glm-5.3", ContextWindow: 1048576, MaxOutputTokens: 8192},
			},
		}
	})
	cat := catalogOf(t, r)

	vision := single(t, cat, nonDefaultProvider+"/kimi-k2.6")
	if got := vision["supports_images"]; got != true {
		t.Errorf("上游声明收图，目录里却是 %#v —— 调用方会按纯文本处理，视觉能力白丢", got)
	}

	plain := single(t, cat, nonDefaultProvider+"/glm-5.3")
	// 未声明 = **没有这个键**。写成 false 会让"上游说不能收图"与
	// "上游没说"变成同一件事，而后者不该被当成否定结论。
	if v, present := plain["supports_images"]; present {
		t.Errorf("上游没声明视觉能力时不该出现 supports_images 键，实际 %#v", v)
	}
}

// TestImageModelAlreadyInCatalogIsMerged 一个模型既是对话模型又是生图模型时
// 只能有一条记录，且两边的声明都要保留。
//
// # 这是真实数据，不是造出来的边界
//
// 实测 loomy 的 `Hy-Image-3.5-preview` 在**两条路径**上都会出现：
// 对话目录里一条（上游给了 context_length=100000、vision=true），
// 生图补充又一条（entryOf(0,0) 于是窗口落到 131072 兜底、且没有视觉声明）。
//
// 同一 id 两条记录、字段还互相矛盾 —— 调用方按 id 建索引时只剩后一条，
// "哪个声明是真的"变成了**顺序依赖**。
func TestImageModelAlreadyInCatalogIsMerged(t *testing.T) {
	const id = "Hy-Image-3.5-preview"
	r := nonDefaultRouter(func(r *imageRouter) {
		r.hasModels = true
		r.genModelsByProvider = map[string][]gateway.ImageModel{
			nonDefaultProvider: {{ID: id, Name: "混元图像 3.5 预览"}},
		}
		r.modelsByProvider = map[string][]gateway.ModelInfo{
			// 对话目录里那条：带上游给的真实窗口与视觉声明。
			nonDefaultProvider: {{ID: id, ContextWindow: 100000, SupportsImages: true}},
		}
	})
	cat := catalogOf(t, r)

	e := single(t, cat, nonDefaultProvider+"/"+id)

	// 保留对话目录那份的真实窗口，而不是生图路径的 131072 兜底。
	if got := e["context_length"]; got != float64(100000) {
		t.Errorf("context_length = %#v，want 100000（对话目录那份才是真值，不是生图路径的兜底）", got)
	}
	// 视觉声明不能被合并过程吃掉。
	if got := e["supports_images"]; got != true {
		t.Errorf("supports_images = %#v，want true", got)
	}
	// 生图能力必须仍然标着 —— 合并不能把它丢了，否则调用方
	// 不知道这个模型该走 /v1/images/generations。
	caps, _ := e["capabilities"].([]any)
	found := false
	for _, c := range caps {
		if c == "image_generation" {
			found = true
		}
	}
	if !found {
		t.Errorf("capabilities = %#v，缺少 image_generation；合并过程把生图能力吃掉了", e["capabilities"])
	}
}

// TestImageModelOnlyAppearsOnceWithCapability 只在生图声明里的模型照旧进目录。
//
// 这是上面那条的**对照**：合并逻辑不能顺手把"目录里没有的生图模型"也吞掉。
func TestImageModelOnlyAppearsOnceWithCapability(t *testing.T) {
	r := nonDefaultRouter(func(r *imageRouter) {
		r.hasModels = true
		r.genModelsByProvider = map[string][]gateway.ImageModel{
			nonDefaultProvider: {{ID: "doubao-seedream-5-lite", Name: "Seedream 5 Lite"}},
		}
		r.modelsByProvider = map[string][]gateway.ModelInfo{
			nonDefaultProvider: {{ID: "glm-5.3", ContextWindow: 1048576}},
		}
	})
	cat := catalogOf(t, r)

	e := single(t, cat, nonDefaultProvider+"/doubao-seedream-5-lite")
	caps, _ := e["capabilities"].([]any)
	if len(caps) != 1 || caps[0] != "image_generation" {
		t.Errorf("capabilities = %#v，want [image_generation]", e["capabilities"])
	}
}

// TestMarkImageGenerationIsIdempotent 重复标记不产生重复的能力位。
//
// 多上游目录合并时同一条记录可能被标记两次 —— 不判重的话 capabilities 里
// 会出现两个 image_generation。
func TestMarkImageGenerationIsIdempotent(t *testing.T) {
	out := []map[string]any{{"id": "loomy/x", "capabilities": []string{"image_generation"}}}
	if !markImageGeneration(out, "loomy/x") {
		t.Fatal("markImageGeneration 没命中已存在的 id")
	}
	caps := out[0]["capabilities"].([]string)
	if len(caps) != 1 {
		t.Errorf("capabilities = %#v，重复标记不该追加", caps)
	}
	// 不存在的 id 返回 false，且不新增记录（调用方据此决定要不要 append）。
	if markImageGeneration(out, "loomy/nope") {
		t.Error("不存在的 id 不该报命中")
	}
	if len(out) != 1 {
		t.Errorf("markImageGeneration 不该新增条目，实际 %d 条", len(out))
	}
	// 已存在但还没有 capabilities 键的记录：应当**建**出这个能力位。
	bare := []map[string]any{{"id": "loomy/y", "context_length": 100000}}
	if !markImageGeneration(bare, "loomy/y") {
		t.Fatal("markImageGeneration 没命中无 capabilities 的记录")
	}
	if caps, _ := bare[0]["capabilities"].([]string); len(caps) != 1 || caps[0] != "image_generation" {
		t.Errorf("capabilities = %#v，want [image_generation]", bare[0]["capabilities"])
	}
}
