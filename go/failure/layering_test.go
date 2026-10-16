package failure_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/xiaolai/sayso/go/failure"
	"github.com/xiaolai/sayso/go/openaihttp"
)

// openaihttp names the same types, so a consumer's openaihttp.Error is a failure.Error.
var _ *failure.Error = (*openaihttp.Error)(nil)

// The app-server client and the credentials classify their failures through this leaf, never through the HTTP
// stack: neither depends on net/http, TLS or the WebSocket library.
func TestCodexPackagesStayOffTheHTTPStack(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("needs the go command")
	}
	out, err := exec.Command(gobin, "list", "-deps", "github.com/xiaolai/sayso/go/codexapp",
		"github.com/xiaolai/sayso/go/codexauth").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "net/http" || dep == "crypto/tls" || strings.HasPrefix(dep, "github.com/coder/websocket") {
			t.Errorf("codexapp or codexauth depends on %s", dep)
		}
	}
}
