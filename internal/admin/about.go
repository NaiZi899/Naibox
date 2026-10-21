package admin

import (
	"encoding/json"
	"net/http"

	"github.com/NaiZi899/Naibox/internal/about"
	"github.com/NaiZi899/Naibox/internal/openurl"
)

// aboutInfo 返回编译期嵌入的关于页数据。
//
// 数据只在编译时存在，运行期没有外部文件依赖；about.json 写坏时
// 只让这一页报错，不影响网关本身。
func (h *Handler) aboutInfo(w http.ResponseWriter, r *http.Request) {
	c, err := about.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "关于页数据损坏: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// openURL 用系统默认浏览器打开一个链接。
//
// 只接受 http/https（校验在 internal/openurl）。控制台是 WebView2，
// 前端 target=_blank 有把页面导航走的风险，所以外链统一走后端。
func (h *Handler) openURL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if err := openurl.Open(body.URL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"opened": true})
}
