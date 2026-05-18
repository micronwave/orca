package schema

// RiskLevel classifies the risk of a goal, obligation, or capsule.
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// Topology is the execution shape selected by the Task Topology Classifier.
// orca.md §7: parallel, test_first, and investigate_then_implement are deferred to Phase 2.
type Topology string

const (
	TopologySingle              Topology = "single"
	TopologyImplementerReviewer Topology = "implementer_reviewer"
	TopologyHumanGated          Topology = "human_gated"
)
