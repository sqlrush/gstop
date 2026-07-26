package gsbench

import "testing"

func TestLockScenarioFactoriesRegisterImplementedCodesOnly(t *testing.T) {
	factories := LockScenarioFactories()
	for code := ScenarioCode(501); code <= 510; code++ {
		if code == 507 {
			continue
		}
		if factories[code] == nil {
			t.Fatalf("missing factory for %d", code)
		}
	}
	for code := ScenarioCode(520); code <= 540; code++ {
		if factories[code] == nil {
			t.Fatalf("missing factory for %d", code)
		}
	}
	for _, code := range []ScenarioCode{507, 511, 512} {
		if factories[code] != nil {
			t.Fatalf("deferred code %d is registered", code)
		}
	}
}
