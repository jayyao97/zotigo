package workspace

import "testing"

func TestStorageSlug(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		fallback string
		want     string
	}{
		{name: "Chinese", input: "中文 项目", fallback: "project", want: "中文-项目"},
		{name: "English", input: "  Hello   World  ", fallback: "project", want: "hello-world"},
		{name: "digits and hyphens", input: "Release-2026---08", fallback: "project", want: "release-2026-08"},
		{name: "Git and path punctuation", input: `Git:/Bad\\Name..lock@{~^:?*[`, fallback: "project", want: "git-bad-name-lock"},
		{name: "empty", input: "", fallback: "project", want: "project"},
		{name: "fully filtered", input: "😀@{..", fallback: "workspace", want: "workspace"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := storageSlug(test.input, test.fallback); got != test.want {
				t.Fatalf("storageSlug(%q, %q) = %q, want %q", test.input, test.fallback, got, test.want)
			}
		})
	}
}

func TestStorageNameUsesStableIDPrefix(t *testing.T) {
	if got := storageName("Readable Name", "project_12345678-abcd-ef00", "project_", "project"); got != "readable-name-12345678" {
		t.Fatalf("project storage name = %q", got)
	}
	if got := storageName("工作区", "workspace_abcdef12-3456-7890", "workspace_", "workspace"); got != "工作区-abcdef12" {
		t.Fatalf("workspace storage name = %q", got)
	}
}
