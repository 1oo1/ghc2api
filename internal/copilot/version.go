package copilot

import "net/http"

// Version 是程序版本号。源码中的默认值随发布更新；
// 发布构建（Docker 镜像）通过 -ldflags "-X ghc2api/internal/copilot.Version=<标签>" 覆盖为 git 标签，因此必须保持为 var。
var Version = "v0.0.2"

// handleVersion 返回程序版本号，供控制台显示。
func handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": Version})
}
