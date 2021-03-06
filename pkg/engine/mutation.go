package engine

import (
	"reflect"
	"strings"
	"time"

	"github.com/golang/glog"
	kyverno "github.com/nirmata/kyverno/pkg/api/kyverno/v1"
	"github.com/nirmata/kyverno/pkg/engine/mutate"
	"github.com/nirmata/kyverno/pkg/engine/response"
	"github.com/nirmata/kyverno/pkg/engine/variables"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	//PodControllers stores the list of Pod-controllers in csv string
	PodControllers = "DaemonSet,Deployment,Job,StatefulSet"
	//PodControllersAnnotation defines the annotation key for Pod-Controllers
	PodControllersAnnotation = "pod-policies.kyverno.io/autogen-controllers"
	//PodTemplateAnnotation defines the annotation key for Pod-Template
	PodTemplateAnnotation = "pod-policies.kyverno.io/autogen-applied"
)

// Mutate performs mutation. Overlay first and then mutation patches
func Mutate(policyContext PolicyContext) (resp response.EngineResponse) {
	startTime := time.Now()
	policy := policyContext.Policy
	resource := policyContext.NewResource
	ctx := policyContext.Context

	startMutateResultResponse(&resp, policy, resource)
	glog.V(4).Infof("started applying mutation rules of policy %q (%v)", policy.Name, startTime)
	defer endMutateResultResponse(&resp, startTime)

	patchedResource := policyContext.NewResource
	for _, rule := range policy.Spec.Rules {
		var ruleResponse response.RuleResponse
		//TODO: to be checked before calling the resources as well
		if !rule.HasMutate() && !strings.Contains(PodControllers, resource.GetKind()) {
			continue
		}
		startTime := time.Now()
		glog.V(4).Infof("Time: Mutate matchAdmissionInfo %v", time.Since(startTime))

		// check if the resource satisfies the filter conditions defined in the rule
		//TODO: this needs to be extracted, to filter the resource so that we can avoid passing resources that
		// dont statisfy a policy rule resource description
		if err := MatchesResourceDescription(resource, rule, policyContext.AdmissionInfo); err != nil {
			glog.V(4).Infof("resource %s/%s does not satisfy the resource description for the rule:\n%s", resource.GetNamespace(), resource.GetName(), err.Error())
			continue
		}

		// operate on the copy of the conditions, as we perform variable substitution
		copyConditions := copyConditions(rule.Conditions)
		// evaluate pre-conditions
		// - handle variable subsitutions
		if !variables.EvaluateConditions(ctx, copyConditions) {
			glog.V(4).Infof("resource %s/%s does not satisfy the conditions for the rule ", resource.GetNamespace(), resource.GetName())
			continue
		}

		mutation := rule.Mutation.DeepCopy()
		// Process Overlay
		if mutation.Overlay != nil {
			overlay := mutation.Overlay
			// subsiitue the variables
			var err error
			if overlay, err = variables.SubstituteVars(ctx, overlay); err != nil {
				// variable subsitution failed
				ruleResponse.Success = false
				ruleResponse.Message = err.Error()
				resp.PolicyResponse.Rules = append(resp.PolicyResponse.Rules, ruleResponse)
				continue
			}

			ruleResponse, patchedResource = mutate.ProcessOverlay(rule.Name, overlay, patchedResource)
			if ruleResponse.Success {
				// - overlay pattern does not match the resource conditions
				if ruleResponse.Patches == nil {
					glog.V(4).Infof(ruleResponse.Message)
					continue
				}

				glog.V(4).Infof("Mutate overlay in rule '%s' successfully applied on %s/%s/%s", rule.Name, resource.GetKind(), resource.GetNamespace(), resource.GetName())
			}

			resp.PolicyResponse.Rules = append(resp.PolicyResponse.Rules, ruleResponse)
			incrementAppliedRuleCount(&resp)
		}

		// Process Patches
		if rule.Mutation.Patches != nil {
			var ruleResponse response.RuleResponse
			ruleResponse, patchedResource = mutate.ProcessPatches(rule, patchedResource)
			glog.Infof("Mutate patches in rule '%s' successfully applied on %s/%s/%s", rule.Name, resource.GetKind(), resource.GetNamespace(), resource.GetName())
			resp.PolicyResponse.Rules = append(resp.PolicyResponse.Rules, ruleResponse)
			incrementAppliedRuleCount(&resp)
		}

		// insert annotation to podtemplate if resource is pod controller
		// skip inserting on existing resource
		if reflect.DeepEqual(policyContext.AdmissionInfo, kyverno.RequestInfo{}) {
			continue
		}

		if strings.Contains(PodControllers, resource.GetKind()) {
			var ruleResponse response.RuleResponse
			ruleResponse, patchedResource = mutate.ProcessOverlay(rule.Name, podTemplateRule, patchedResource)
			if !ruleResponse.Success {
				glog.Errorf("Failed to insert annotation to podTemplate of %s/%s/%s: %s", resource.GetKind(), resource.GetNamespace(), resource.GetName(), ruleResponse.Message)
				continue
			}

			if ruleResponse.Success && ruleResponse.Patches != nil {
				glog.V(2).Infof("Inserted annotation to podTemplate of %s/%s/%s: %s", resource.GetKind(), resource.GetNamespace(), resource.GetName(), ruleResponse.Message)
				resp.PolicyResponse.Rules = append(resp.PolicyResponse.Rules, ruleResponse)
			}
		}
	}
	// send the patched resource
	resp.PatchedResource = patchedResource
	return resp
}
func incrementAppliedRuleCount(resp *response.EngineResponse) {
	resp.PolicyResponse.RulesAppliedCount++
}

func startMutateResultResponse(resp *response.EngineResponse, policy kyverno.ClusterPolicy, resource unstructured.Unstructured) {
	// set policy information
	resp.PolicyResponse.Policy = policy.Name
	// resource details
	resp.PolicyResponse.Resource.Name = resource.GetName()
	resp.PolicyResponse.Resource.Namespace = resource.GetNamespace()
	resp.PolicyResponse.Resource.Kind = resource.GetKind()
	resp.PolicyResponse.Resource.APIVersion = resource.GetAPIVersion()
	// TODO(shuting): set response with mutationFailureAction
}

func endMutateResultResponse(resp *response.EngineResponse, startTime time.Time) {
	resp.PolicyResponse.ProcessingTime = time.Since(startTime)
	glog.V(4).Infof("finished applying mutation rules policy %v (%v)", resp.PolicyResponse.Policy, resp.PolicyResponse.ProcessingTime)
	glog.V(4).Infof("Mutation Rules appplied count %v for policy %q", resp.PolicyResponse.RulesAppliedCount, resp.PolicyResponse.Policy)
}

// podTemplateRule mutate pod template with annotation
// pod-policies.kyverno.io/autogen-applied=true
var podTemplateRule = kyverno.Rule{
	Name: "autogen-annotate-podtemplate",
	Mutation: kyverno.Mutation{
		Overlay: map[string]interface{}{
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"annotations": map[string]interface{}{
							"+(pod-policies.kyverno.io/autogen-applied)": "true",
						},
					},
				},
			},
		},
	},
}
