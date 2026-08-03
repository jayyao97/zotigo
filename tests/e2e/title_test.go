//go:build e2e

package e2e

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/services"
	"github.com/jayyao97/zotigo/core/testutil"
)

// TestE2E_DeepSeekTitleProviderSmoke verifies the isolated title request against
// one configured DeepSeek profile. It is intentionally opt-in because it uses
// live provider credentials.
func TestE2E_DeepSeekTitleProviderSmoke(t *testing.T) {
	e2eCfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Skipf("E2E config not available: %v", err)
	}
	profiles := deepSeekProfiles(e2eCfg.AllProfiles())
	if len(profiles) == 0 {
		t.Skip("No DeepSeek profiles configured")
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	profile := profiles[names[0]]
	if profile.APIKey == "" {
		t.Skipf("No API key for profile %s", names[0])
	}

	provider, err := providers.NewProvider(profile)
	if err != nil {
		t.Fatalf("NewProvider(%s): %v", names[0], err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	title, err := services.GenerateTitle(
		ctx,
		provider,
		"帮我看看这个配置为什么启动失败",
		"定位到了：Viper 将 profiles 的 map key 转成了小写，但 default_profile 保留了大写字母，导致区分大小写的查找失败。",
	)
	if err != nil {
		t.Fatalf("GenerateTitle(%s): %v", names[0], err)
	}
	if strings.TrimSpace(title) == "" || strings.Contains(title, "\n") {
		t.Fatalf("invalid title: %q", title)
	}
	if utf8.RuneCountInString(title) > 60 {
		t.Fatalf("title exceeds 60 runes: %q", title)
	}
	t.Logf("Profile: %s, title: %s", names[0], title)
}
