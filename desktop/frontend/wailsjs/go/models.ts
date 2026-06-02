export namespace main {
	
	export class BudgetSummary {
	    total_tokens_spent: number;
	    total_wall_time_seconds: number;
	    total_tool_calls: number;
	    total_retries: number;
	    total_obligations_discharged: number;
	    total_patches_accepted: number;
	    total_patches_rejected: number;
	
	    static createFrom(source: any = {}) {
	        return new BudgetSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.total_tokens_spent = source["total_tokens_spent"];
	        this.total_wall_time_seconds = source["total_wall_time_seconds"];
	        this.total_tool_calls = source["total_tool_calls"];
	        this.total_retries = source["total_retries"];
	        this.total_obligations_discharged = source["total_obligations_discharged"];
	        this.total_patches_accepted = source["total_patches_accepted"];
	        this.total_patches_rejected = source["total_patches_rejected"];
	    }
	}
	export class BudgetView {
	    budget_id: string;
	    goal_id: string;
	    capsule_id: string;
	    obligation_id: string;
	    tokens_spent: number;
	    wall_time_seconds: number;
	    tool_calls: number;
	    retries: number;
	    obligations_discharged: number;
	    patches_accepted: number;
	    patches_rejected: number;
	
	    static createFrom(source: any = {}) {
	        return new BudgetView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.budget_id = source["budget_id"];
	        this.goal_id = source["goal_id"];
	        this.capsule_id = source["capsule_id"];
	        this.obligation_id = source["obligation_id"];
	        this.tokens_spent = source["tokens_spent"];
	        this.wall_time_seconds = source["wall_time_seconds"];
	        this.tool_calls = source["tool_calls"];
	        this.retries = source["retries"];
	        this.obligations_discharged = source["obligations_discharged"];
	        this.patches_accepted = source["patches_accepted"];
	        this.patches_rejected = source["patches_rejected"];
	    }
	}
	export class CapsuleView {
	    capsule_id: string;
	    agent: string;
	    role: string;
	    state: string;
	    worktree_path: string;
	    max_tokens: number;
	    max_wall_time_seconds: number;
	    topology_decision_id: string;
	
	    static createFrom(source: any = {}) {
	        return new CapsuleView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.capsule_id = source["capsule_id"];
	        this.agent = source["agent"];
	        this.role = source["role"];
	        this.state = source["state"];
	        this.worktree_path = source["worktree_path"];
	        this.max_tokens = source["max_tokens"];
	        this.max_wall_time_seconds = source["max_wall_time_seconds"];
	        this.topology_decision_id = source["topology_decision_id"];
	    }
	}
	export class ConditionView {
	    id: string;
	    description: string;
	    status: string;
	
	    static createFrom(source: any = {}) {
	        return new ConditionView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.description = source["description"];
	        this.status = source["status"];
	    }
	}
	export class DecisionView {
	    decision_id: string;
	    context: string;
	    decision: string;
	    rationale: string;
	    made_by: string;
	    related_ids: string[];
	    // Go type: time
	    created_at: any;
	
	    static createFrom(source: any = {}) {
	        return new DecisionView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.decision_id = source["decision_id"];
	        this.context = source["context"];
	        this.decision = source["decision"];
	        this.rationale = source["rationale"];
	        this.made_by = source["made_by"];
	        this.related_ids = source["related_ids"];
	        this.created_at = this.convertValues(source["created_at"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class EvidenceView {
	    evidence_id: string;
	    type: string;
	    source: string;
	    command: string;
	    exit_code: number;
	    summary: string;
	    raw_log_path: string;
	    inline_output: string;
	    supports: string[];
	    reused_from_id: string;
	    // Go type: time
	    created_at: any;
	
	    static createFrom(source: any = {}) {
	        return new EvidenceView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.evidence_id = source["evidence_id"];
	        this.type = source["type"];
	        this.source = source["source"];
	        this.command = source["command"];
	        this.exit_code = source["exit_code"];
	        this.summary = source["summary"];
	        this.raw_log_path = source["raw_log_path"];
	        this.inline_output = source["inline_output"];
	        this.supports = source["supports"];
	        this.reused_from_id = source["reused_from_id"];
	        this.created_at = this.convertValues(source["created_at"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class FailureView {
	    failure_id: string;
	    source_capsule_id: string;
	    failure_type: string;
	    summary: string;
	    affected_files: string[];
	    error_signature: string;
	    prior_attempt_count: number;
	    recommended_next_action: string;
	
	    static createFrom(source: any = {}) {
	        return new FailureView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.failure_id = source["failure_id"];
	        this.source_capsule_id = source["source_capsule_id"];
	        this.failure_type = source["failure_type"];
	        this.summary = source["summary"];
	        this.affected_files = source["affected_files"];
	        this.error_signature = source["error_signature"];
	        this.prior_attempt_count = source["prior_attempt_count"];
	        this.recommended_next_action = source["recommended_next_action"];
	    }
	}
	export class GoalView {
	    goal_id: string;
	    original_intent: string;
	    status: string;
	    risk_level: string;
	    conditions: ConditionView[];
	    // Go type: time
	    created_at: any;
	
	    static createFrom(source: any = {}) {
	        return new GoalView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.goal_id = source["goal_id"];
	        this.original_intent = source["original_intent"];
	        this.status = source["status"];
	        this.risk_level = source["risk_level"];
	        this.conditions = this.convertValues(source["conditions"], ConditionView);
	        this.created_at = this.convertValues(source["created_at"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ObligationView {
	    obligation_id: string;
	    goal_condition_id: string;
	    description: string;
	    status: string;
	    blocking: boolean;
	    risk_level: string;
	    satisfied_by: string[];
	
	    static createFrom(source: any = {}) {
	        return new ObligationView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.obligation_id = source["obligation_id"];
	        this.goal_condition_id = source["goal_condition_id"];
	        this.description = source["description"];
	        this.status = source["status"];
	        this.blocking = source["blocking"];
	        this.risk_level = source["risk_level"];
	        this.satisfied_by = source["satisfied_by"];
	    }
	}
	export class PatchView {
	    patch_id: string;
	    capsule_id: string;
	    status: string;
	    summary: string;
	    changed_files: string[];
	    obligation_ids_claimed: string[];
	    tokens_used: number;
	    wall_time_seconds: number;
	    base_commit: string;
	    diff_path: string;
	
	    static createFrom(source: any = {}) {
	        return new PatchView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.patch_id = source["patch_id"];
	        this.capsule_id = source["capsule_id"];
	        this.status = source["status"];
	        this.summary = source["summary"];
	        this.changed_files = source["changed_files"];
	        this.obligation_ids_claimed = source["obligation_ids_claimed"];
	        this.tokens_used = source["tokens_used"];
	        this.wall_time_seconds = source["wall_time_seconds"];
	        this.base_commit = source["base_commit"];
	        this.diff_path = source["diff_path"];
	    }
	}
	export class PendingGate {
	    gate_type: string;
	    related_id: string;
	    reason: string;

	    static createFrom(source: any = {}) {
	        return new PendingGate(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.gate_type = source["gate_type"];
	        this.related_id = source["related_id"];
	        this.reason = source["reason"];
	    }
	}
	export class SetupHealthView {
	    config_exists: boolean;
	    event_log_exists: boolean;
	    warning: string;

	    static createFrom(source: any = {}) {
	        return new SetupHealthView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.config_exists = source["config_exists"];
	        this.event_log_exists = source["event_log_exists"];
	        this.warning = source["warning"];
	    }
	}
	export class TimelineEntry {
	    // Go type: time
	    at: any;
	    type: string;
	    summary: string;
	    status: string;

	    static createFrom(source: any = {}) {
	        return new TimelineEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.at = this.convertValues(source["at"], null);
	        this.type = source["type"];
	        this.summary = source["summary"];
	        this.status = source["status"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

