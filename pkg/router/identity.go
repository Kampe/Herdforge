package router

import (
	"fmt"
	"strings"
)

// VerifyModelIdentity is the post-launch fence. A successful process start is
// not proof that the provider honored the requested model; callers must pass
// the model/family observed from the live session and reject mismatches.
func VerifyModelIdentity(provider, model, family, observedProvider, observedModel, observedFamily string) error {
	wantProvider := strings.ToLower(strings.TrimSpace(provider))
	wantModel := strings.TrimSpace(model)
	wantFamily := strings.ToLower(strings.TrimSpace(family))
	gotProvider := strings.ToLower(strings.TrimSpace(observedProvider))
	gotModel := strings.TrimSpace(observedModel)
	gotFamily := strings.ToLower(strings.TrimSpace(observedFamily))
	if wantProvider == "" || wantModel == "" || wantFamily == "" {
		return fmt.Errorf("route identity is incomplete")
	}
	if gotProvider == "" || gotModel == "" || gotFamily == "" {
		return fmt.Errorf("post-launch route identity is incomplete: provider=%q model=%q family=%q", observedProvider, observedModel, observedFamily)
	}
	if wantProvider != gotProvider || wantModel != gotModel || wantFamily != gotFamily {
		return fmt.Errorf("post-launch route identity mismatch: got %s/%s/%s want %s/%s/%s", gotProvider, gotModel, gotFamily, wantProvider, wantModel, wantFamily)
	}
	return nil
}
