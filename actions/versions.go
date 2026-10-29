package actions

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

// SteveFeature identifies version-dependent behavior.
type SteveFeature string

const (
	SteveCaseInsensitiveSort        SteveFeature = "case-insensitive SQL sorting"
	SteveStructuredSummary          SteveFeature = "structured summary counts"
	SteveSummaryOptions             SteveFeature = "summaryonly and summarynamespaced"
	SteveRestartAgeSort             SteveFeature = "Pod restart-age sorting"
	SteveNamespacedPatch            SteveFeature = "namespaced PATCH namespace fix"
	SteveReplicaSetAssociatedData   SteveFeature = "ReplicaSet associated Pod data"
	SteveAdditionalFieldIndexes     SteveFeature = "CRD group, singular name, and resource description indexes"
	SteveBasicSummary               SteveFeature = "basic SQL summaries"
	SteveArrayMembershipFilters     SteveFeature = "CONTAINS and NOTCONTAINS filters"
	StevePodRestartIndexes          SteveFeature = "Pod nested restart indexes"
	SteveNumericConfigMapCount      SteveFeature = "numeric ConfigMap DATA count"
	SteveIngressMissingHost         SteveFeature = "Ingress rules with missing hosts"
	SteveKindNamePrefix             SteveFeature = "kind-derived generated name prefix"
	SteveSecretTypeAlias            SteveFeature = "Secret _type query index"
	SteveFutureRevisionValidation   SteveFeature = "future revision validation"
	SteveNonListableCollectionGuard SteveFeature = "SQL guard for non-listable collections"
	SteveCollectionRevision         SteveFeature = "SQL collection revision"
	SteveArrayElementIndexes        SteveFeature = "indexed array-element queries"
	SteveNumericFieldIndexes        SteveFeature = "int64 and float64 field indexes"
	SteveRawDateColumns             SteveFeature = "raw CRD date columns"
	StevePrinterColumnFieldMapping  SteveFeature = "CRD printer-column field mapping"
	SteveMultipleLabelFilterSort    SteveFeature = "label sorting with multiple label filters"
)

type steveFeatureRule struct {
	minimum   string
	backports []steveBackport
}

// Backports apply only within their minor release line.
type steveBackport struct {
	minimum string
	before  string // First release where the backport was removed, if any.
}

var steveFeatureRules = map[SteveFeature]steveFeatureRule{
	SteveCaseInsensitiveSort:        {minimum: "v0.9.16"},
	SteveStructuredSummary:          {minimum: "v0.9.11"},
	SteveSummaryOptions:             {minimum: "v0.9.11"},
	SteveRestartAgeSort:             {minimum: "v0.9.6"},
	SteveNamespacedPatch:            {minimum: "v0.9.15"},
	SteveReplicaSetAssociatedData:   {minimum: "v0.10.1"},
	SteveAdditionalFieldIndexes:     {minimum: "v0.10.8"},
	SteveBasicSummary:               {minimum: "v0.8.1"},
	SteveArrayMembershipFilters:     {minimum: "v0.8.18"},
	StevePodRestartIndexes:          {minimum: "v0.8.15"},
	SteveNumericConfigMapCount:      {minimum: "v0.7.20", backports: []steveBackport{{minimum: "v0.6.50"}}},
	SteveIngressMissingHost:         {minimum: "v0.6.6"},
	SteveKindNamePrefix:             {minimum: "v0.10.2", backports: []steveBackport{{minimum: "v0.8.26"}, {minimum: "v0.9.19"}}},
	SteveSecretTypeAlias:            {minimum: "v0.8.2"},
	SteveFutureRevisionValidation:   {minimum: "v0.7.19", backports: []steveBackport{{minimum: "v0.6.49", before: "v0.6.51"}}},
	SteveNonListableCollectionGuard: {minimum: "v0.8.13", backports: []steveBackport{{minimum: "v0.7.39"}}},
	SteveCollectionRevision:         {minimum: "v0.6.8"},
	SteveArrayElementIndexes:        {minimum: "v0.6.15"},
	SteveNumericFieldIndexes:        {minimum: "v0.6.14"},
	SteveRawDateColumns:             {minimum: "v0.8.2", backports: []steveBackport{{minimum: "v0.7.42"}}},
	StevePrinterColumnFieldMapping:  {minimum: "v0.8.2", backports: []steveBackport{{minimum: "v0.7.42"}}},
	SteveMultipleLabelFilterSort:    {minimum: "v0.6.22"},
}

var steveReleaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-(?:alpha|beta|rc)(?:\.[0-9]+)?)?$`)

// SteveSupports checks STEVE_VERSION; unknown targets keep all checks enabled.
func SteveSupports(feature SteveFeature) bool {
	return steveSupportsVersion(strings.TrimSpace(os.Getenv("STEVE_VERSION")), feature)
}

func steveSupportsVersion(version string, feature SteveFeature) bool {
	rule, known := steveFeatureRules[feature]
	if !known || !steveReleaseVersion.MatchString(version) || !semver.IsValid(version) {
		return true
	}
	minimum := rule.minimum
	for _, backport := range rule.backports {
		if semver.MajorMinor(version) == semver.MajorMinor(backport.minimum) {
			if backport.before != "" && semver.Compare(version, backport.before+"-0") >= 0 {
				return false
			}
			minimum = backport.minimum
			break
		}
	}
	// Include prereleases of the first supporting release.
	return semver.Compare(version, minimum+"-0") >= 0
}

// RequireSteveFeature skips checks unsupported by the target release.
func RequireSteveFeature(t *testing.T, feature SteveFeature) {
	t.Helper()
	version := strings.TrimSpace(os.Getenv("STEVE_VERSION"))
	if !steveSupportsVersion(version, feature) {
		rule := steveFeatureRules[feature]
		supported := rule.minimum + "+"
		for _, backport := range rule.backports {
			supported += " or " + backport.minimum + "+"
			if backport.before != "" {
				supported += " before " + backport.before
			}
			supported += " within " + semver.MajorMinor(backport.minimum)
		}
		t.Skipf("%s requires Steve %s (including introduction-patch release prereleases); target is %s",
			feature, supported, version)
	}
}
