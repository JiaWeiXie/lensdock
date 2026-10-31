package product

import "testing"

func TestServerProfileRejectsAmbiguousSandboxAssignments(t *testing.T) {
	settings, err := profileFor(ProfileServer)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"LENSDOCK_CHROMIUM_SANDBOX=true\nLENSDOCK_CHROMIUM_SANDBOX=false\n",
		"LENSDOCK_CHROMIUM_SANDBOX=false\nLENSDOCK_CHROMIUM_SANDBOX=true\n",
		"LENSDOCK_CHROMIUM_SANDBOX=true\nLENSDOCK_CHROMIUM_SANDBOX=true\n",
		"LENSDOCK_CHROMIUM_SANDBOX=true\nexport LENSDOCK_CHROMIUM_SANDBOX=false\n",
		"LENSDOCK_CHROMIUM_SANDBOX=true\nLENSDOCK_CHROMIUM_SANDBOX = false\n",
		"LENSDOCK_CHROMIUM_SANDBOX=true\nexport\tLENSDOCK_CHROMIUM_SANDBOX=false\n",
		"LENSDOCK_CHROMIUM_SANDBOX=true\nLENSDOCK_CHROMIUM_SANDBOX: false\n",
		"LENSDOCK_CHROMIUM_SANDBOX=true\nLENSDOCK_CHROMIUM_SANDBOX=\"false\"\n",
		"OTHER='multiline\nLENSDOCK_CHROMIUM_SANDBOX=true\n'\n",
		"LENSDOCK_CHROMIUM_SANDBOX=true\nOTHER=\"multiline\nLENSDOCK_CHROMIUM_SANDBOX=false\n\"\n",
		"LENSDOCK_CHROMIUM_SANDBOX=${OVERRIDE:-true}\n",
	} {
		if err := settings.checkRuntimeEnv(text); err == nil {
			t.Error("ambiguous sandbox configuration accepted")
		}
	}
	if err := settings.checkRuntimeEnv("# unrelated settings are preserved\nOTHER=value\nLENSDOCK_CHROMIUM_SANDBOX=true\n"); err != nil {
		t.Fatal(err)
	}
}
