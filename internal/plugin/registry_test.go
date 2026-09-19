package plugin

import (
	"strings"
	"testing"
)

func TestLoadRejectsPluginURLUserinfoWithoutLeakingIt(t *testing.T) {
	const secret = "api-token"
	t.Setenv("PLUGIN_SERVICE", "https://local-user:"+secret+"@service.internal")

	_, err := Load(nil)
	if err == nil {
		t.Fatal("expected plugin URL userinfo to be rejected")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("plugin URL password must not be repeated in the error")
	}
}
