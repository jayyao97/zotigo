package zotigod

import (
	"slices"
	"testing"
)

func TestWorkerProcessEnvReplacesInheritedAuthToken(t *testing.T) {
	got := workerProcessEnv([]string{
		"PATH=/bin",
		workerAuthTokenEnv + "=stale-secret",
	}, "current-secret")
	want := []string{"PATH=/bin", workerAuthTokenEnv + "=current-secret"}
	if !slices.Equal(got, want) {
		t.Fatalf("workerProcessEnv = %#v, want %#v", got, want)
	}
}
