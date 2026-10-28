package maxima

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"xodus-nexus-wails/assets"
)

func TestEmbeddedPatchesMatchLock(t *testing.T) {
	if len(SourceLock.Patches) == 0 || len(SourceLock.CargoLockSHA256) != 64 {
		t.Fatalf("lock = %+v", SourceLock)
	}
	for _, patch := range SourceLock.Patches {
		data, err := assets.Maxima.ReadFile("maxima/" + patch.File)
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != patch.SHA256 {
			t.Errorf("%s checksum differs from the lock", patch.File)
		}
	}
	if data, err := assets.Maxima.ReadFile("maxima/Cargo.lock"); err != nil {
		t.Fatal(err)
	} else if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != SourceLock.CargoLockSHA256 {
		t.Error("Cargo.lock checksum differs from the lock")
	}
	for _, file := range []string{"maxima/LICENSE.GPL-3.0", "maxima/README.md"} {
		if _, err := assets.Maxima.ReadFile(file); err != nil {
			t.Errorf("%s is not embedded: %v", file, err)
		}
	}
}

func TestStripANSIAndLastLine(t *testing.T) {
	output := "\x1b[32mINFO\x1b[37m - Logged in as player!\n{\"event\":\"progress\"}\nERROR - no build\n"
	if got := stripANSI(output); loggedInAs.FindStringSubmatch(got)[1] != "player" {
		t.Errorf("account not parsed from %q", got)
	}
	if got := lastLine(output); got != "ERROR - no build" {
		t.Errorf("last line = %q", got)
	}
}
