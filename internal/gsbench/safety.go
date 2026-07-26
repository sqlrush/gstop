package gsbench

import "fmt"

// AuthorizeScenario verifies that the selected scenario has the explicit
// safeguards required by its risk level before it can run.
func AuthorizeScenario(def ScenarioDefinition, cfg BenchConfig, options CLIOptions, env Environment) error {
	switch def.Risk {
	case RiskA:
		return nil
	case RiskB:
		if !cfg.Safety.AllowAdminMutation {
			return fmt.Errorf("scenario %d risk B requires safety.allow_admin_mutation=true", def.Code)
		}
		if riskLevelRank(options.AllowRisk) < riskLevelRank(RiskB) {
			return fmt.Errorf("scenario %d risk B requires --allow-risk B or C", def.Code)
		}
		return nil
	case RiskC:
		if !cfg.Safety.AllowInfrastructureFault {
			return fmt.Errorf("scenario %d risk C requires safety.allow_infrastructure_fault=true", def.Code)
		}
		if options.AllowRisk != RiskC {
			return fmt.Errorf("scenario %d risk C requires --allow-risk C", def.Code)
		}
		if !env.Capabilities[CapabilityExternalFaultProvider] {
			return fmt.Errorf("scenario %d risk C requires external fault provider capability", def.Code)
		}
		return nil
	default:
		return fmt.Errorf("scenario %d has unknown risk level %q", def.Code, def.Risk)
	}
}

func riskLevelRank(risk RiskLevel) int {
	switch risk {
	case RiskA:
		return 1
	case RiskB:
		return 2
	case RiskC:
		return 3
	default:
		return 0
	}
}
