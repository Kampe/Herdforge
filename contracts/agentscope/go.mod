// Module github.com/Kampe/Herdforge/contracts/agentscope is a lightweight,
// independently consumable nested Go module owned by Herdforge. It isolates
// the AgentScope and trusted-job contract types, canonicalization, digests,
// and validation so consumers do not pull in the root Herdforge module's
// (large, unrelated) dependency cascade. The versioned JSON schemas live
// under ./v1alpha1 and are read by this package's tests.
module github.com/Kampe/Herdforge/contracts/agentscope

go 1.25.0

require (
	github.com/dlclark/regexp2 v1.12.0
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.1
)

require golang.org/x/text v0.39.0 // indirect
