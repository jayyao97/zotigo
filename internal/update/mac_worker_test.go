package update

import (
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Exercise the actual bootstrap plist and its shell command without touching
// the user's launchd domain. Finish jobs after the next plist overwrites theirs.
func TestMacWorkerUnloadsItsOwnLabelAfterExit(t *testing.T) {
	dir := t.TempDir()
	ctl := filepath.Join(dir, "launchctl")
	if err := os.WriteFile(ctl, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CALLS\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	callLog := filepath.Join(dir, "calls")
	t.Setenv("CALLS", callLog)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	domain := "gui/" + strconv.Itoa(os.Getuid())
	var commands [][]string
	var expected []string
	actions := []string{"prepare", "apply", "prepare", "apply"}
	for index, action := range actions {
		label := "zotigo-update-" + strconv.Itoa(index)
		// A failed worker also needs to release its job.
		args := []string{"/bin/sh", "-c", `printf '%s\n' "$1" >> "$CALLS"; exit "$2"`, "worker", action + " <&>", strconv.Itoa(index)}
		if err := launchMacWorker(label, args, dir); err != nil {
			t.Fatal(err)
		}
		plist, err := os.ReadFile(filepath.Join(dir, "worker.plist"))
		if err != nil {
			t.Fatal(err)
		}
		var job struct {
			Label string   `xml:"dict>string"`
			Args  []string `xml:"dict>array>string"`
		}
		if err := xml.Unmarshal(plist, &job); err != nil || job.Label != label || len(job.Args) == 0 {
			t.Fatalf("invalid job %s: %v", plist, err)
		}
		commands = append(commands, job.Args)
		expected = append(expected, "bootstrap "+domain+" "+filepath.Join(dir, "worker.plist"))
	}
	for index, command := range commands {
		if out, err := exec.Command(command[0], command[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		expected = append(expected, actions[index]+" <&>", "bootout "+domain+"/zotigo-update-"+strconv.Itoa(index))
	}
	got, err := os.ReadFile(callLog)
	if err != nil || string(got) != strings.Join(expected, "\n")+"\n" {
		t.Fatalf("launchctl/worker calls: %s (%v)", got, err)
	}
}
