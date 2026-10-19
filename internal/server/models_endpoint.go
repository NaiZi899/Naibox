// models_endpoint.go /v1/models（含 /v1/model）的目录构建。
//
// # 为什么单独一个文件
//
// 这一段是"控制台性能"事故的核心：多上游目录构建走的是**同步、串行、无缓存**
// 的上游调用（见 modelList 第 2 步）。把它从 2400 行的 handler.go 里摘出来，
// 是为了让"目录构建"这个关注点有独立的可读面与独立的测试面 ——
// 也是本次给每个上游加缓存/TTL/并发/超时的落点。
//
// 内容与拆分前**逐字节相同**（纯搬移），行为零变化。

package server

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/gateway"
)

// staticModels 动态接口失败时的**回退模型表**。
//
// # 为什么留在这里而不是搬走
//
// 这张表是 workbuddy 的模型清单（`owned_by: "workbuddy"`），
// 严格说是上游数据。但它的作用是"上游拉不到时的兜底展示" ——
// 一个纯静态、无副作用的常量，且 /v1/models 是出口层职责。
//
// 多上游落地后这里要改成**按 provider 合并**（Task 6/8）：
// 每个 Provider 通过 Models() 给出自己的目录，出口层合并并加前缀。
// 本任务（阶段 0）只做解耦，不改行为 —— 所以暂时保留原表，
// 但把它与 owned_by 抽成可注入字段，避免核心继续**硬编码**上游名字。
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "hy3", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "context_length": 131072},
}

// DefaultUpstream 默认上游的客户端（装配层注入）。
//
// # 为什么核心只收接口，不收 *upstream.Client
//
// `*upstream.Client` 是 **workbuddy 私有 SDK 的类型**。核心包（出口层）
// 认识它，就等于核心依赖某个具体上游 —— 正是 arch_test 判据 3 禁止的、
// 也是 privateSDKKnownDebt 里 server 那条债的形态。
//
// 改成接口之后，**判据与行为一个字没变**（方法签名逐字对齐），
// 变的只是"核心认识的是一个形状，而不是某个包"。
//
// # 方法集是怎么定出来的（不是照抄 Client 的全部方法）
//
// 只列出口层**真正调用**的四个：FetchModels / FetchModelCatalog /
// RefreshToken / ChatStreamWithIP。Client 上其余方法（签到、成长、旅行、
// 上报…）是 workbuddy 的业务动作，出口层从不碰。
//
// 窄接口的好处不只是"看着干净"：它让"出口层到底需要上游提供什么"
// 变成可读的事实。宽接口会掩盖这一点 —— 后来者会以为出口层依赖全部。
type DefaultUpstream interface {
	// FetchModels 拉默认上游的动态模型目录。
	FetchModels(a *auth.Auth) ([]DefaultModel, error)

	// FetchModelCatalog 拉默认上游的模型目录（含倍率）。
	FetchModelCatalog(a *auth.Auth) (*gateway.ModelCatalog, error)

	// RefreshToken 续期一个凭证。
	//
	// 失败时**应当**返回 `*RefreshError`（装配层的适配器负责包装）。
	// 出口层据此区分"session 已死"（计数门控后禁用）与一般失败
	//（只记一次错）。返回其它错误类型时一律按一般失败处理 ——
	// 那是**保守方向**：不会误禁用账号。
	RefreshToken(a *auth.Auth) error

	// ChatStreamWithIP 发起一次对话流。
	ChatStreamWithIP(a *auth.Auth, body []byte, clientIP string) (rc io.ReadCloser, status int, respBody []byte, err error)
}

// DefaultModel 出口层需要的模型元信息（中立形状）。
//
// # 为什么不用 gateway.ModelInfo
//
// `gateway.ModelInfo` 的窗口字段是 `int`，而默认上游给的是 `int64`
// （上游用 int64 存 token 数）。改 gateway 的字段类型会牵动所有上游的
// 实现与契约测试 —— 那是为了让一个**内部投影**少写两个字段，
// 代价与收益不成比例。
//
// 所以这里保留 int64：出口层拿到后按需 `int64(...)` 提升
// （`entryOf` 本来就收 int64）。适配层做一次转换即可。
//
// ⚠ 刻意**不**收 `Name` / `Efforts`：出口层从不读它们
// （模型名直接用 ID；思考档位由出站改写层处理）。
// 收进来只会让"接口比消费者需要的宽"，而宽接口会掩盖真实依赖。
//
// SupportsImages 是**唯一**后来加进来的字段，理由是出口层真的读它：
// 它要进 /v1/models 的 `supports_images` 声明（见 entryOf）。加字段的判据
// 与本注释开头同一条 —— 有消费者才算收。
type DefaultModel struct {
	ID             string
	ContextWindow  int64
	MaxTokens      int64
	SupportsImages bool
}

// defaultUpstreamAdapter 把 *upstream.Client 适配成 DefaultUpstream。
//
// # 为什么适配器在装配层（cmd/server）而不是这里
//
// 本文件在 corePackages 里，**不得** import 任何具体上游。
// 适配器要同时认识两边，只能住在能同时 import 它们的地方 ——
// 装配层正是那个地方（与 poolAdapter / workbuddyLogin 同一手法）。
//
// 本文件只定义接口；`cmd/server` 提供实现。这样"核心要什么"与
// "上游给什么"的转换发生在唯一的接缝上，而不是散进核心。

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []DefaultModel
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
//
// # 多上游下的形状（目录收窄：一个上游只列一条 provider/model）
//
// 每个上游的模型**只以 "provider/model" 列一条**，没有任何裸名记录：
//
//	{"id":"workbuddy/glm-5.2", "owned_by":"workbuddy"}
//	{"id":"codearts/GLM-5.2",  "owned_by":"codearts"}
//
// 收窄之前默认上游是"裸名 + 带前缀"两份（workbuddy 的 16 个模型列成 32 条），
// 同一个模型在下拉框里出现两次。用户明确要求"统一、不要重复"，
// 所以默认上游那一支现在**只 append 带前缀的那一份**。
//
// # 这次收窄的代价（如实记录，不是"没有影响"）
//
// 选路侧没变：providerFor（provider_router.go）对裸名仍按默认上游解析，
// 老客户端把 "glm-5.2" 填进 model 打进来照样能用。变的是**列目录**：
// 既有客户端若从 /v1/models 里取 id 再回填，收窄后会看不到裸名，
// 必须改用 "provider/model" 形式。
//
// 顺序：默认上游的条目仍排在最前（既有客户端取 data[0] 的习惯），
// 其后是其它上游（按字典序）。
//
// # def == "" 的边界（多上游但默认上游为空）
//
// 没有上游身份就拼不出 "provider/model"，硬拼只会得到 "/glm-5.2" ——
// 一个没人能解析的畸形 id。所以 def == "" 时保持**裸名**（每个模型仍只有
// 一条，只是不做前缀投影），且绝不产生以 "/" 开头的 id。
//
// # 未注入路由时（Provider == nil）
//
// 完全退化成改造前的单上游行为：**只有裸名，没有任何前缀记录**。
// 这是既有测试与既有部署的路径，字节级不变。
func (h *Handler) modelList() []map[string]any {
	// 单上游模式：与改造前逐字节一致。
	if h.cfg.Provider == nil {
		return h.singleProviderModelList(h.ownedBy())
	}

	def := h.defaultProvider()
	out := make([]map[string]any, 0, 16)

	// 1. 默认上游：只列带前缀的一份 —— 收窄后不再同时列裸名，
	//    否则同一个模型会在目录里出现两次（用户报的就是这个重复）。
	//    def == "" 是边界：拼前缀只会得到 "/glm-5.2"，
	//    所以此时保持裸名 —— 每个模型仍然只有一条，只是没有上游前缀。
	for _, e := range h.singleProviderModelList(h.ownedByFor(def)) {
		if def == "" {
			out = append(out, e)
			continue
		}
		out = append(out, prefixed(e, def))
	}

	// 2. 其余上游：只有带前缀的一份。
	//    顺序上排在默认上游之后 —— 既有客户端取 data[0] 时拿到的仍是它熟悉的那个。
	//
	// # 取数方式变了，顺序没变（本次性能修复的落点）
	//
	// 原来这里是对 h.cfg.Provider.Models(context.Background(), id) 的**串行、
	// 无缓存、无超时**调用：端点耗时 = 各上游耗时**之和**，且每次请求都重打
	// 一遍上游（实测 loomy 是重尾：p90 10094ms / max 27497ms，而 /v1/models
	// p50 已到 4107ms、控制台其它端点全在 1-14ms）。
	//
	// 现在走 providerDirectories：并发取（耗时从"之和"变"最大值"）、
	// 按上游 1h 缓存、单上游 5s 超时。三件事都在 models_endpoint_cache.go。
	//
	// ⚠ 遍历的仍是 otherProviders(def) 的**原顺序**（默认上游在前、其余字典序），
	// 只是从 map 里取各自的结果 —— providerDirectories 刻意不排序，顺序这件事
	// 只有这一处说了算。
	otherIDs := h.otherProviders(def)
	dirs := h.providerDirectories(otherIDs)
	for _, id := range otherIDs {
		// 拿不到目录（没号/失败/超时）即跳过该上游，不拖垮整个 /v1/models：
		// 与改造前 `if !ok || len(infos) == 0 { continue }` 同一判据
		//（取不到的 id 不在 map 里，取到空列表的也不进去 —— 两者都表现为 len==0）。
		infos := dirs[id]
		if len(infos) == 0 {
			continue
		}
		for _, mi := range infos {
			out = append(out, prefixed(h.entryOf(mi.ID, int64(mi.ContextWindow), int64(mi.MaxOutputTokens), mi.SupportsImages, h.ownedByFor(id)), id))
		}
	}

	// 3. **生图模型**（默认上游 + 其余上游）。
	//
	// # 为什么要单独一步（它们不在上面的目录里）
	//
	// 上游的 `/models` 目录**不含**两个生图模型，或者含了也被对话语境的
	// 过滤逻辑剔除（见 loomy/models.go 的 Unavailable 注释：那个标记
	// 只说明"不能走 chat"）。而 `/v1/images/generations` 恰恰只能用它们 ——
	// 不进目录，调用方就不知道能生图、也不知道该填哪个 model。
	//
	// 用接口断言发现可选能力（与 providerIDs 同一范式）：不实现
	// ImageModelExt 的上游在这里被静默跳过，行为与改造前一致。
	//
	// ⚠ 补充前先记下**已经在目录里的 id**：同一个模型可能既在对话目录里、
	// 又在生图声明里（实测 loomy/Hy-Image-3.5-preview）。重复列两条会让
	// 同 id 出现两份互相矛盾的声明（窗口 100000 vs 131072、
	// supports_images true vs 无），调用方按 id 建索引时只剩一条，
	// 行为变成顺序依赖。命中的走 markImageGeneration 就地合并。
	seenID := make(map[string]bool, len(out))
	for _, e := range out {
		if v := asString(e["id"]); v != "" {
			seenID[v] = true
		}
	}
	imageIDs := append([]string{def}, h.otherProviders(def)...)
	for _, id := range imageIDs {
		for _, im := range h.imageModelsOf(id) {
			full := im.ID
			if id != "" {
				full = id + "/" + im.ID
			}
			if seenID[full] {
				markImageGeneration(out, full)
				continue
			}
			seenID[full] = true
			e := h.entryOf(im.ID, 0, 0, false, h.ownedByFor(id))
			// 标出生图能力：调用方据此判断"这个模型要用
			// /v1/images/generations，不是 chat/completions"。
			// 非标准字段（OpenAI 没有这个概念），但**不标就无法区分**
			// —— 目录里两个模型看名字都知道是生图，但程序不该靠名字猜。
			e["capabilities"] = []string{"image_generation"}
			if id != "" {
				out = append(out, prefixed(e, id))
				continue
			}
			out = append(out, e)
		}
	}
	return out
}

// imageModelsOf 问装配层要某上游声明的生图模型。
//
// 与 providerIDs() 同一范式（可选接口断言）：没实现 ImageModelExt
// 或本部署没接线时返回 nil，调用方静默跳过。
func (h *Handler) imageModelsOf(id string) []gateway.ImageModel {
	if h.cfg.Provider == nil {
		return nil
	}
	lookup, ok := h.cfg.Provider.(interface {
		ImageModels(id string) ([]gateway.ImageModel, bool)
	})
	if !ok {
		return nil
	}
	list, ok := lookup.ImageModels(id)
	if !ok {
		return nil
	}
	return list
}

// imageModelEntries 把某上游声明的生图模型编成目录条目。
//
// # 为什么两条路径都要调它（单上游 / 多上游）
//
// 生图模型**不在**上游的对话目录里（它们走 /chat/completions 会 404，
// 见 loomy/models.go 的 Unavailable 注释）。所以无论部署是单上游还是多上游，
// 都必须在目录里**补一次** —— 漏了任何一条路径，那种部署下
// `/v1/images/generations` 就是"端点在、但没人知道该填哪个 model"。
//
// id 为空串表示"不带前缀"（单上游模式）；非空时加 `<id>/` 前缀
// （与其余条目一致：前缀是网关的路由记号）。
func (h *Handler) imageModelEntries(id, owned string) []map[string]any {
	list := h.imageModelsOf(id)
	if len(list) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, im := range list {
		e := h.entryOf(im.ID, 0, 0, false, owned)
		// 标出生图能力：调用方据此知道这个模型要走
		// /v1/images/generations 而不是 chat/completions。
		//
		// 为什么不靠名字猜（"seedream"/"image" 看起来就像生图）：
		// 名字是上游可以随时改的展示字符串，而这是**能力声明**。
		// 用名字判断的代码在上游改名那天会静默错。
		e["capabilities"] = []string{"image_generation"}
		if name := strings.TrimSpace(im.Name); name != "" {
			e["name"] = name
		}
		if id != "" {
			e = prefixed(e, id)
		}
		out = append(out, e)
	}
	return out
}

// singleProviderModelList 单上游路径：动态列表优先，失败回退静态表。
// owned 是填进 owned_by 的值。
func (h *Handler) singleProviderModelList(owned string) []map[string]any {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			out = append(out, h.entryOf(mi.ID, mi.ContextWindow, mi.MaxTokens, mi.SupportsImages, owned))
		}
		return out
	}
	// 静态回退表：补上 owned_by（表里不再硬编码上游名）
	out := make([]map[string]any, 0, len(staticModels))
	for _, m := range staticModels {
		e := make(map[string]any, len(m)+1)
		for k, v := range m {
			e[k] = v
		}
		e["owned_by"] = owned
		out = append(out, e)
	}
	return out
}

// entryOf 把模型元信息编成 OpenAI 形状的一条记录。
//
// 刻意收**四个标量**而不是收一个结构体：两个来源（upstream.ModelInfo 与
// gateway.ModelInfo）是两个包的具名类型，收结构体会逼本函数选边站，
// 从而让 server 依赖其中一个的具体类型。收标量则两边都能用。
//
// 窗口/token 参数是 int64 —— upstream.ModelInfo 用 int64、gateway.ModelInfo
// 用 int，int64 是两者的公共超集，调用处各自隐式提升即可。
//
// # supports_images 只在 true 时出现
//
// 与窗口的 `context_length` 不同，这里**没有兜底值**：窗口的 131072 是
// "客户端需要一个正数才能算预算"的工程妥协，而"能不能收图"给个猜测值
// 会让调用方发一张必然被上游拒的图 —— 那种失败对用户表现为"这个模型坏了"。
// 所以 false 一律省略，语义是"上游没有声明支持"，调用方按纯文本处理。
func (h *Handler) entryOf(id string, ctxWindow, maxTokens int64, supportsImages bool, owned string) map[string]any {
	entry := map[string]any{
		"id":                id,
		"object":            "model",
		"created":           1753600000,
		"owned_by":          owned,
		"context_length":    ctxWindow,
		"max_output_tokens": maxTokens,
	}
	if ctxWindow == 0 {
		entry["context_length"] = 131072 // 兜底
	}
	if supportsImages {
		entry["supports_images"] = true
	}
	return entry
}

// markImageGeneration 给目录里**已经存在**的那条记录补上生图能力标记。
//
// 生图补充与对话目录会指向同一个模型（实测 `loomy/Hy-Image-3.5-preview`：
// 对话目录里一条，生图补充又一条）。原实现直接 append，于是目录里出现
// 两条同 id 记录，且两条的 context_length / supports_images 还不一样 ——
// 调用方按 id 建索引时只剩后一条，"哪个声明是真的"变成了顺序依赖。
//
// 现在的处理：**保留对话目录那条**（它带上游给的真实窗口与视觉声明），
// 只把"能走 images/generations"这个事实并进去。返回是否命中。
func markImageGeneration(out []map[string]any, id string) bool {
	for _, e := range out {
		if asString(e["id"]) != id {
			continue
		}
		if caps, ok := e["capabilities"].([]string); ok {
			for _, c := range caps {
				if c == "image_generation" {
					return true
				}
			}
			e["capabilities"] = append(caps, "image_generation")
			return true
		}
		e["capabilities"] = []string{"image_generation"}
		return true
	}
	return false
}

// otherProviders 返回除默认上游外的已注册上游（按字典序，稳定输出）。
//
// 返回空切片时 modelList 退化成"只有默认上游" ——
// 这正是"codearts 未启用"时应当发生的事。
func (h *Handler) otherProviders(def string) []string {
	all := h.providerIDs()
	out := make([]string, 0, len(all))
	for _, id := range all {
		if id == def {
			continue
		}
		out = append(out, id)
	}
	return out
}

// providerIDs 返回已知的全部上游 ID。
//
// 优先问路由（它是唯一的权威）；路由没提供枚举能力时回落成"只有默认上游"，
// 因为一个不枚举的上游集合无法安全地猜测。
func (h *Handler) providerIDs() []string {
	if h.cfg.Provider == nil {
		return nil
	}
	if l, ok := h.cfg.Provider.(interface{ IDs() []string }); ok {
		return l.IDs()
	}
	return nil
}

// ownedBy 返回默认上游在 /v1/models 里的 owned_by 值。
//
// 未注入时给 "local" —— 一个**不假装知道**是哪个上游的中性值。
func (h *Handler) ownedBy() string {
	return h.ownedByFor(h.defaultProvider())
}

// ownedByFor 返回指定上游的 owned_by：用它自己的 ID ——
// 每条模型的 `owned_by` 反映**实际上游**，而不是全局一个值。
//
// 默认上游且装配层显式给了 OwnedBy 时以 OwnedBy 为准
// （那是改造前就存在的注入点，行为要保持）。
// 没有上游上下文时回落 "local"。
func (h *Handler) ownedByFor(id string) string {
	if id == "" {
		if h.cfg.OwnedBy != "" {
			return h.cfg.OwnedBy
		}
		return "local"
	}
	if id == h.defaultProvider() && h.cfg.OwnedBy != "" {
		return h.cfg.OwnedBy
	}
	return id
}

// fetchDynamicModels 从池中任一健康账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
func (h *Handler) fetchDynamicModels() []DefaultModel {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	// ⚠ P1（本次修复）：选号必须带 **provider 维度**。
	//
	// 原来这里是 `h.cfg.Pool.Pick()` —— **不带 provider 过滤**，于是在多上游
	// 部署里可能抽到 codearts 的账号，然后拿它去调 workbuddy 的 FetchModels。
	// 失败之后下面那句 `Pool.NoteError(acct.UID)` 会把**一个无辜的 codearts
	// 账号**喂进熔断计数（err_total +1，连续 3 次就熔断），而真正的原因
	// （用错了上游的客户端）没有任何痕迹。
	//
	// 这与本次修的 chat 路径是同一个 bug 的两种形态：**选号按 provider 分域了，
	// 出站调用没有**。chat 路径换成按 Provider 分派；这里没有"多上游目录"的
	// 需求（/v1/models 的动态列表只属于默认上游），所以最小且正确的修法是
	// 取号时限定在默认上游。
	//
	// ⚠ 传 **""（不是 h.defaultProvider()）**：本函数用的客户端恒是
	// h.cfg.Upstream（默认上游的），所以候选集必须是**池子眼里的默认上游**。
	//
	// 池子的默认上游由 `SetDefaultProvider` 在启动时注入（cmd/server），
	// 而 `h.cfg.DefaultProvider` 是出口层的配置值 —— 两者**可能不一致**
	// （测试里就是：池子没注入、出口层配了 "workbuddy"）。传后者会让
	// `normalizeProvider` 选出一个池子里根本不存在的域，候选集为空、
	// 动态目录静默消失（实测：32 条基线掉到 27 条 —— 静默退回了静态表）。
	//
	// "" 的语义正是"没有上游上下文，走池子的默认"（见 normalizeProvider），
	// 与改造前 `Pick()` 的候选集**完全一致**，只是把"不带过滤"换成了
	// "按默认上游过滤"。两者在单上游部署里是同一个集合，因此既有部署零变化。
	acct := h.cfg.Pool.PickForExcluding("", nil)
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败惩罚该账号，避免下次 Pick 又选中同一个反复失败；lastFail 保持全局负缓存。
		h.cfg.Pool.NoteError(acct.UID)
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}
