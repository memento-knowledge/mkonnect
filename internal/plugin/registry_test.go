package plugin

import (
	"strings"
	"testing"
)

func TestLoadRejectsCredentialBearingPluginURLWithoutLeakingIt(t *testing.T) {
	const secret = "api-token"
	urls := []string{
		"https://local-user:" + secret + "@service.internal",
		"https://service.internal/?access_token=" + secret,
		"https://service.internal/#access_token=" + secret,
	}

	for _, pluginURL := range urls {
		t.Run(pluginURL, func(t *testing.T) {
			t.Setenv("PLUGIN_SERVICE", pluginURL)

			_, err := Load(nil)
			if err == nil {
				t.Fatal("expected credential-bearing plugin URL to be rejected")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("plugin URL credential must not be repeated in the error")
			}
		})
	}
}
