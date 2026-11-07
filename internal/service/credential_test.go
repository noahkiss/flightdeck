package service

import (
	"strings"
	"testing"
)

// The job files take the locale from the environment and nothing else: a Claude token or an API
// key in the environment of gff-hab install never reaches a unit or a plist
// (capcom-service.md 10.2).
func TestJobFilesHoldNoCredential(t *testing.T) {
	const planted = "planted-credential-41c0"
	environ := []string{"LANG=xx_YY.UTF-8", "CLAUDE_CODE_OAUTH_TOKEN=" + planted, "ANTHROPIC_API_KEY=" + planted, "LC_ALL=" + "C"}
	for _, in := range []Init{Systemd, Launchd} {
		c := testConfig(in, t.TempDir())
		c.Locale = LocaleEnv(environ)
		files, err := Render(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.Contains(string(f.Content), planted) || strings.Contains(string(f.Content), "CLAUDE_CODE_OAUTH_TOKEN") {
				t.Fatalf("%s %s holds the credential:\n%s", in, f.Path, f.Content)
			}
		}
	}
}
