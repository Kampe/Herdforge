package verifier

import "testing"

func TestHermeticEnvironmentCarriesOnlyTrustedContainmentAuthority(t *testing.T) {
	for _, test := range []struct {
		name, parent, want string
	}{
		{name: "parent marker", parent: "1", want: hermeticContainerEnv + "=1"},
		{name: "parent absent", parent: "", want: ""},
		{name: "parent other", parent: "unexpected", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(hermeticContainerEnv, test.parent)
			for _, entry := range hermeticEnvironment() {
				if entry == test.want && test.want != "" {
					return
				}
				if len(entry) >= len(hermeticContainerEnv)+1 && entry[:len(hermeticContainerEnv)+1] == hermeticContainerEnv+"=" {
					t.Fatalf("unexpected containment authority %q for parent %q", entry, test.parent)
				}
			}
			if test.want != "" {
				t.Fatalf("missing propagated containment authority %q", test.want)
			}
		})
	}
}
