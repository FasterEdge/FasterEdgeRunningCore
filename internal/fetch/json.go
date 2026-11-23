package fetch

import (
	"encoding/json"
	"io"
)

// decodeJSON 读一个 JSON 对象。限制读取量，避免异常巨大的响应把内存吃满。
func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(io.LimitReader(r, 8<<20)).Decode(v)
}
