package launcher

import (
	"testing"

	"xodus-nexus-wails/internal/shared"
)

func TestMatchEATitle(t *testing.T) {
	library := []shared.EATitle{{Name: "Dead Space™", OfferID: "remake"}, {Name: "Dead Space 2", OfferID: "ds2"}, {Name: "Mass Effect™ Legendary Edition", OfferID: "me"},
		{Name: "Titanfall 2", OfferID: "tf2"}, {Name: "Dragon Age 2", OfferID: "da2"}}
	for title, want := range map[string]string{"Dead Space": "remake", "Dead Space 2": "ds2", "Mass Effect Legendary Edition": "me", "Dead Space - 2008": "", "Dead Space 3": "",
		"Titanfall® 2: Ultimate Edition": "tf2", "Dragon Age™ 2 - Digital Deluxe Edition": "da2", "Dragon Age™: Inquisition - Game of the Year Edition": "", "Mass Effect": ""} {
		got, ok := matchEATitle(library, title)
		if (want == "") == ok || got.OfferID != want {
			t.Errorf("%q matched %+v, want %q", title, got, want)
		}
	}
}

func TestEALaunchEnvironment(t *testing.T) {
	env, err := eaLaunchEnvironment(shared.Game{LaunchTools: &shared.GameLaunchTools{CPULimit: 4, LaunchOptions: "DXVK_HUD=fps"}})
	if err != nil || env["WINE_CPU_TOPOLOGY"] != "4:0,1,2,3" || env["DXVK_HUD"] != "fps" {
		t.Errorf("env = %v, %v", env, err)
	}
	env, err = eaLaunchEnvironment(shared.Game{LaunchTools: &shared.GameLaunchTools{CPULimit: 4, LaunchOptions: "WINE_CPU_TOPOLOGY=2:0,1"}})
	if err != nil || env["WINE_CPU_TOPOLOGY"] != "2:0,1" {
		t.Errorf("explicit topology replaced: %v, %v", env, err)
	}
	if env, err := eaLaunchEnvironment(shared.Game{}); err != nil || len(env) != 0 {
		t.Errorf("defaults = %v, %v", env, err)
	}
	if !legacyEAOffer("OFB-EAST:55619") || legacyEAOffer("Origin.OFR.50.0004049") {
		t.Error("legacy offer detection")
	}
}
