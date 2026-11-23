package supervise

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 控制端口的探测很短：目标是“问一句还活着吗”，不是做长任务。
// 超时短一点，装载/卸载流程才不会因为一个挂死的管理器而卡住。
const controlTimeout = 3 * time.Second

// 停止被托管进程要等它收尾：DontCrack 的 /shutdown 会先发中断信号、等 5 秒
// 再强杀，整个请求本身就可能要好几秒。给它更宽的预算，否则卸载会假失败。
const controlStopTimeout = 30 * time.Second

var (
	controlClient     = &http.Client{Timeout: controlTimeout}
	controlStopClient = &http.Client{Timeout: controlStopTimeout}
)

// httpGet 对自托管管理器的控制端口做一次 GET。
func httpGet(url string) error {
	resp, err := controlClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s 返回 %s", url, resp.Status)
	}
	return nil
}

// httpPost 对自托管管理器的控制端口做一次 POST。
func httpPost(url string) error {
	resp, err := controlStopClient.Post(url, "", strings.NewReader(""))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s 返回 %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
