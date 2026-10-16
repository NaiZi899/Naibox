// errorexplain_ext.go 第八个扩展点：上游自报「这个错误该怎么解释给人听」。
//
// # 为什么需要它（用户诉求）
//
// 用户原话：「给每个 key 加一个状态来显示 key 的健康，比如返回了无余额或者
// 欠费和其他报错等，对应的都有状态码解读」。
//
// 账号池已经有 `status` 列（正常/冷却/禁用），也有 `reason` 字段。但那个
// reason 是**核心写的通用文案**（"额度不足"、"429 rate limit"）——
// 它是核心对**分类**的描述，不是上游对**那个具体错误**的描述。
//
// 差别在用户能看到的粒度上：
//
//	核心 reason : "额度不足"                    ← 我知道分类是 HardCredit
//	上游解读    : "402 INSUFFICIENT_BALANCE ——
//	               预付费余额已耗尽。该 key 需充值后才会恢复；
//	               冷却到 22:11 是为了不再每小时内白撞 60 次。"
//
// 后者才是用户能据此**行动**的信息（去充值 / 换 key / 等一等）。
//
// # 为什么由上游报，而不是核心写一张表
//
// 这是本项目最硬的判据（见 AccountColumnsExt / DisplayNameExt 的注释）：
// **核心不认识任何具体上游**。状态码是各上游自定义的字符串枚举
// （基元律动是 `INSUFFICIENT_BALANCE`，workbuddy 是数字 12153，
// codearts 是 `InferHub.4291`）—— 核心写不出来，只能由上游报。
//
// 前端也因此**零改动**：它只渲染上游给的文本，不认识任何上游名。
//
// # 与 ErrorClassifier 的分工
//
//	ErrorClassifier.Classify  决定**怎么办**（冷却/禁用/只换号）—— 机器读
//	ErrorExplainExt.Explain   解释**为什么**（给人看的中文）—— 人读
//
// 两者**必须来自同一份判据**，否则会出现"因为余额不足被冷却，但界面上
// 写的是参数错误"这种自相矛盾。所以实现通常放在同一个文件里，
// 共用同一组关键词常量（见 jiyuan/errorclassifier.go）。
package gateway

// ErrorExplain 一条错误解读（给用户看的中文）。
//
// 刻意只有文本，没有结构化字段（没有 code/kind/severity）：
// 上游能提供的最有价值的东西是**它自己写的一句话**，而任何结构化字段
// 都会立刻引出"前端怎么渲染这个 severity"的问题 —— 那是表现层的事，
// 不该由上游决定。文本由前端原样放入 title/提示里。
type ErrorExplain struct {
	// Code 上游自己的错误码（原样，不做归一化）。
	// 例：`INSUFFICIENT_BALANCE`、`12153`、`InferHub.4291`。
	// 空串表示"上游没有可识别的码，只有 HTTP 状态码"。
	Code string
	// HTTPStatus 触发这个解读的 HTTP 状态码（0 = 与状态码无关）。
	HTTPStatus int
	// Summary 一句话结论（简短，会显示在状态单元格里）。
	// 例："余额不足"、"key 已失效"、"触发限流"。
	Summary string
	// Detail 详细解读（可多行）：为什么会这样、该怎么办。
	// 例："预付费余额已耗尽。该 key 需充值后才会恢复。"
	Detail string
	// Kind 这份解读对应的分类（与 ErrorClassifier 的返回**必须一致**）。
	//
	// 冗余地带上它，是为了让"解释"与"处置"的对应关系可被**断言**：
	// 契约测试会检查 Explain 给出的 Kind 与 Classify 对同一输入的结果相同。
	// 没有它的话，"因为余额不足被冷却、界面却说参数错误"这种漂移无人能发现。
	Kind ErrorKind
}

// ErrorExplainExt 上游自报「我的错误体怎么读给人听」。
//
// # 不实现它就是"没有解读"
//
// 核心回落为：状态列只显示通用状态（正常/冷却/禁用）+ 冷却原因。
// 不实现**完全正常** —— 绝大多数上游的错误体对用户没有额外信息量。
//
// 用 `ExtOf` 的存在性区分"没实现"与"实现了但返回零值"：
// 后者是明确说"这个错误我认识但没什么可说的"。
type ErrorExplainExt interface {
	// Explain 按 HTTP 状态码 + 响应体给出解读。
	//
	// 返回值 ok=false 表示"我不认识这个错误" —— 调用方回落通用文案。
	// **不要**为了"总有话说"而返回一个编造的解读：那正是本文件要消除的
	// "看起来有信息、实际误导人"的形态。
	//
	// ⚠ 实现约束与 ErrorClassifier.Classify **逐字相同**：
	//   - 必须是纯函数：不发网络请求、不读写共享状态
	//   - 不得 panic
	//   - 传入的 body 已被调用方限制过长度
	Explain(status int, body string) (ErrorExplain, bool)
}
