package llm

import (
	"encoding/json"
	"net/http"
)

// jsonNewDecoder 从 HTTP 请求体解出一个 JSON decoder（测试共用小工具）。
func jsonNewDecoder(r *http.Request) *json.Decoder {
	return json.NewDecoder(r.Body)
}
