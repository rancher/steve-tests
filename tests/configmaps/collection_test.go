package configmaps

import (
	"testing"

	"github.com/rancher/steve-tests/actions"
)

// Each row owns a namespace. A case label also excludes controller-created resources.
type collectionSeed struct {
	name   string
	labels map[string]string
}

func TestConfigMapsCollectionScenarios(t *testing.T) {
	t.Parallel()
	cl := getTestClient(t)
	tests := []struct {
		name        string
		sql         bool
		seeds       []collectionSeed
		query       string
		want        actions.CollectionExpectation
		legacyOrder []string
	}{
		{name: "configmap collection scenario POC",
			seeds: []collectionSeed{
				{name: "poc-alpha", labels: map[string]string{"steve-test-poc": "include"}},
				{name: "poc-beta", labels: map[string]string{"steve-test-poc": "include"}},
				{name: "poc-ignore", labels: map[string]string{"steve-test-poc": "ignore"}},
			},
			query: "filter=metadata.labels.steve-test-poc=include&sort=-metadata.name&pagesize=2",
			want:  actions.CollectionExpectation{Order: []string{"poc-beta", "poc-alpha"}},
		},
		{name: "configmap name sort ascending with pagesize",
			seeds: []collectionSeed{
				{name: "cm-zulu", labels: map[string]string{"steve-test-poc": "name-sort"}},
				{name: "cm-alpha", labels: map[string]string{"steve-test-poc": "name-sort"}},
				{name: "cm-mid", labels: map[string]string{"steve-test-poc": "name-sort"}},
				{name: "cm-skip", labels: map[string]string{"steve-test-poc": "other"}},
			},
			query: "filter=metadata.labels.steve-test-poc=name-sort&sort=metadata.name&pagesize=2",
			want:  actions.CollectionExpectation{Order: []string{"cm-alpha", "cm-mid"}},
		},
		{name: "configmap filter with no matches yields empty",
			seeds: []collectionSeed{
				{name: "cm-one", labels: map[string]string{"steve-test-poc": "will-not-match"}},
			},
			query: "filter=metadata.labels.steve-test-poc=nonexistent-value&pagesize=10",
			want:  actions.CollectionExpectation{Order: []string{}},
		},
		{name: "configmap contains check after label filter",
			seeds: []collectionSeed{
				{name: "keep-1", labels: map[string]string{"extra": "yes", "steve-test-poc": "keep"}},
				{name: "keep-2", labels: map[string]string{"steve-test-poc": "keep"}},
				{name: "drop-1", labels: map[string]string{"steve-test-poc": "drop"}},
			},
			query: "filter=metadata.labels.steve-test-poc=keep",
			want:  actions.CollectionExpectation{Length: new(2), Contains: []string{"keep-1", "keep-2"}},
		},
		{name: "configmap OR filter matches either label value",
			seeds: []collectionSeed{
				{name: "or-red", labels: map[string]string{"steve-test-poc": "red"}},
				{name: "or-blue", labels: map[string]string{"steve-test-poc": "blue"}},
				{name: "or-green", labels: map[string]string{"steve-test-poc": "green"}},
			},
			query: "filter=metadata.labels.steve-test-poc=red,metadata.labels.steve-test-poc=blue&sort=metadata.name",
			want:  actions.CollectionExpectation{Order: []string{"or-blue", "or-red"}},
		},
		{name: "configmap multi column sort by group label then reverse name",
			sql: true,
			seeds: []collectionSeed{
				{name: "ms-a-1", labels: map[string]string{"group": "a", "steve-test-poc": "multi-sort"}},
				{name: "ms-a-2", labels: map[string]string{"group": "a", "steve-test-poc": "multi-sort"}},
				{name: "ms-b-1", labels: map[string]string{"group": "b", "steve-test-poc": "multi-sort"}},
			},
			query: "filter=metadata.labels.steve-test-poc=multi-sort&sort=metadata.labels[group],-metadata.name",
			want:  actions.CollectionExpectation{Order: []string{"ms-a-2", "ms-a-1", "ms-b-1"}},
		},
		{name: "configmap label sort ascending uses version collation",
			sql: true,
			seeds: []collectionSeed{
				{name: "label-alpha", labels: map[string]string{"mixed-case-sort": "alpha", "steve-test-poc": "label-case-sort"}},
				{name: "label-bravo", labels: map[string]string{"mixed-case-sort": "Bravo", "steve-test-poc": "label-case-sort"}},
				{name: "label-charlie", labels: map[string]string{"mixed-case-sort": "charlie", "steve-test-poc": "label-case-sort"}},
			},
			query:       "filter=metadata.labels.steve-test-poc=label-case-sort&sort=metadata.labels[mixed-case-sort]",
			want:        actions.CollectionExpectation{Order: []string{"label-alpha", "label-bravo", "label-charlie"}},
			legacyOrder: []string{"label-bravo", "label-alpha", "label-charlie"},
		},
		{name: "configmap label sort descending uses version collation",
			sql: true,
			seeds: []collectionSeed{
				{name: "label-desc-alpha", labels: map[string]string{"mixed-case-sort": "alpha", "steve-test-poc": "label-case-sort-desc"}},
				{name: "label-desc-bravo", labels: map[string]string{"mixed-case-sort": "Bravo", "steve-test-poc": "label-case-sort-desc"}},
				{name: "label-desc-charlie", labels: map[string]string{"mixed-case-sort": "charlie", "steve-test-poc": "label-case-sort-desc"}},
			},
			query:       "filter=metadata.labels.steve-test-poc=label-case-sort-desc&sort=-metadata.labels[mixed-case-sort]",
			want:        actions.CollectionExpectation{Order: []string{"label-desc-charlie", "label-desc-bravo", "label-desc-alpha"}},
			legacyOrder: []string{"label-desc-charlie", "label-desc-alpha", "label-desc-bravo"},
		},
		{name: "configmap optional label sort ascending retains unlabeled rows",
			sql: true,
			seeds: []collectionSeed{
				{name: "optional-label-bravo", labels: map[string]string{"optional-rank": "bravo", "steve-test-poc": "optional-label-asc"}},
				{name: "optional-label-missing", labels: map[string]string{"steve-test-poc": "optional-label-asc"}},
				{name: "optional-label-alpha", labels: map[string]string{"optional-rank": "alpha", "steve-test-poc": "optional-label-asc"}},
			},
			query: "filter=metadata.labels.steve-test-poc=optional-label-asc&sort=metadata.labels[optional-rank],metadata.name",
			want:  actions.CollectionExpectation{Order: []string{"optional-label-alpha", "optional-label-bravo", "optional-label-missing"}},
		},
		{name: "configmap optional label sort descending retains unlabeled rows",
			sql: true,
			seeds: []collectionSeed{
				{name: "optional-label-desc-bravo", labels: map[string]string{"optional-rank": "bravo", "steve-test-poc": "optional-label-desc"}},
				{name: "optional-label-desc-missing", labels: map[string]string{"steve-test-poc": "optional-label-desc"}},
				{name: "optional-label-desc-alpha", labels: map[string]string{"optional-rank": "alpha", "steve-test-poc": "optional-label-desc"}},
			},
			query: "filter=metadata.labels.steve-test-poc=optional-label-desc&sort=-metadata.labels[optional-rank],metadata.name",
			want:  actions.CollectionExpectation{Order: []string{"optional-label-desc-missing", "optional-label-desc-bravo", "optional-label-desc-alpha"}},
		},
		{name: "configmap second page returns the remainder",
			seeds: []collectionSeed{
				{name: "pg-01", labels: map[string]string{"steve-test-poc": "paging"}},
				{name: "pg-02", labels: map[string]string{"steve-test-poc": "paging"}},
				{name: "pg-03", labels: map[string]string{"steve-test-poc": "paging"}},
			},
			query: "filter=metadata.labels.steve-test-poc=paging&sort=metadata.name&pagesize=2&page=2",
			want:  actions.CollectionExpectation{Order: []string{"pg-03"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.sql {
				actions.RequireSQLMode(t)
			}
			ns := actions.CreateTestNamespace(t, cl)
			resources := cl.Resource("configmap").Namespace(ns)
			for _, seed := range test.seeds {
				labels := map[string]string{actions.CaseLabel: "collection"}
				for key, value := range seed.labels {
					labels[key] = value
				}
				actions.CreateAndWait(t, resources, seed.name, newConfigMapBody(ns, seed.name, labels))
			}
			query := "filter=metadata.labels." + actions.CaseLabel + "=collection&" + test.query
			if test.sql && !actions.SteveSupports(actions.SteveMultipleLabelFilterSort) {
				// Older SQL joins reuse aliases when two label filters are
				// combined with a label sort. Each row owns its namespace, and
				// its existing steve-test-poc filter already selects its seeds.
				query = test.query
				t.Log("Checking label sorting with one fixture-selecting label filter")
			}
			want := test.want
			if test.legacyOrder != nil && !actions.SteveSupports(actions.SteveCaseInsensitiveSort) {
				want.Order = test.legacyOrder
			}
			actions.WaitForCollection(t, resources, query, want)
		})
	}
}

func TestConfigMapsFilterOperatorScenarios(t *testing.T) {
	t.Parallel()
	cl := actions.RequireSQLMode(t).Client
	tests := []struct {
		name  string
		seeds []collectionSeed
		query string
		want  actions.CollectionExpectation
	}{
		{name: "configmap not-equal operator excludes the matching value",
			seeds: []collectionSeed{
				{name: "ne-red", labels: map[string]string{"steve-test-poc": "red"}},
				{name: "ne-blue", labels: map[string]string{"steve-test-poc": "blue"}},
				{name: "ne-green", labels: map[string]string{"steve-test-poc": "green"}},
			},
			query: "filter=metadata.labels.steve-test-poc!=red&sort=metadata.name",
			want:  actions.CollectionExpectation{Order: []string{"ne-blue", "ne-green"}},
		},
		{name: "configmap tilde operator does substring match on name",
			seeds: []collectionSeed{
				{name: "needle-target", labels: map[string]string{"steve-test-poc": "tilde"}},
				{name: "haystack-one", labels: map[string]string{"steve-test-poc": "tilde"}},
				{name: "haystack-two", labels: map[string]string{"steve-test-poc": "tilde"}},
			},
			query: "filter=metadata.name~needle",
			want:  actions.CollectionExpectation{Length: new(1), Contains: []string{"needle-target"}},
		},
		{name: "configmap negated tilde operator excludes substring matches",
			seeds: []collectionSeed{
				{name: "needle-excluded", labels: map[string]string{"steve-test-poc": "not-tilde"}},
				{name: "haystack-kept-a", labels: map[string]string{"steve-test-poc": "not-tilde"}},
				{name: "haystack-kept-b", labels: map[string]string{"steve-test-poc": "not-tilde"}},
			},
			query: "filter=metadata.name!~needle&sort=metadata.name",
			want:  actions.CollectionExpectation{Order: []string{"haystack-kept-a", "haystack-kept-b"}},
		},
		{name: "configmap in-set operator matches set members",
			seeds: []collectionSeed{
				{name: "set-red", labels: map[string]string{"steve-test-poc": "red"}},
				{name: "set-blue", labels: map[string]string{"steve-test-poc": "blue"}},
				{name: "set-green", labels: map[string]string{"steve-test-poc": "green"}},
			},
			query: "filter=metadata.labels.steve-test-poc%20in%20(red,%20blue)&sort=metadata.name",
			want:  actions.CollectionExpectation{Order: []string{"set-blue", "set-red"}},
		},
		{name: "configmap notin-set operator excludes set members",
			seeds: []collectionSeed{
				{name: "ni-red", labels: map[string]string{"steve-test-poc": "red"}},
				{name: "ni-blue", labels: map[string]string{"steve-test-poc": "blue"}},
				{name: "ni-green", labels: map[string]string{"steve-test-poc": "green"}},
			},
			query: "filter=metadata.labels.steve-test-poc%20notin%20(red,%20blue)",
			want:  actions.CollectionExpectation{Length: new(1), Contains: []string{"ni-green"}},
		},
		{name: "configmap OR across different fields matches either condition",
			seeds: []collectionSeed{
				{name: "orf-by-name", labels: map[string]string{"steve-test-poc": "skip-a"}},
				{name: "orf-by-label", labels: map[string]string{"steve-test-poc": "pick"}},
				{name: "orf-neither", labels: map[string]string{"steve-test-poc": "skip-b"}},
			},
			query: "filter=metadata.name~orf-by-name,metadata.labels.steve-test-poc=pick&sort=metadata.name",
			want:  actions.CollectionExpectation{Order: []string{"orf-by-label", "orf-by-name"}},
		},
		{name: "configmap tilde operator is case-insensitive while equals is case-sensitive",
			seeds: []collectionSeed{
				{name: "ci-needle-target", labels: map[string]string{"steve-test-poc": "case-check"}},
				{name: "ci-plain", labels: map[string]string{"steve-test-poc": "case-check"}},
			},
			query: "filter=metadata.name~NEEDLE",
			want:  actions.CollectionExpectation{Length: new(1), Contains: []string{"ci-needle-target"}},
		},
		{name: "configmap label exists operator keeps only labelled items",
			seeds: []collectionSeed{
				{name: "exists-labelled", labels: map[string]string{"steve-test-extra": "present", "steve-test-poc": "exists"}},
				{name: "exists-plain", labels: map[string]string{"steve-test-poc": "exists"}},
			},
			query: "filter=metadata.labels[steve-test-extra]",
			want:  actions.CollectionExpectation{Length: new(1), Contains: []string{"exists-labelled"}},
		},
		{name: "configmap negated label exists operator drops labelled items",
			seeds: []collectionSeed{
				{name: "notexists-labelled", labels: map[string]string{"steve-test-extra": "present", "steve-test-poc": "not-exists"}},
				{name: "notexists-plain", labels: map[string]string{"steve-test-poc": "not-exists"}},
			},
			query: "filter=!metadata.labels[steve-test-extra]",
			want:  actions.CollectionExpectation{Length: new(1), Contains: []string{"notexists-plain"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ns := actions.CreateTestNamespace(t, cl)
			resources := cl.Resource("configmap").Namespace(ns)
			for _, seed := range test.seeds {
				labels := map[string]string{actions.CaseLabel: "collection"}
				for key, value := range seed.labels {
					labels[key] = value
				}
				actions.CreateAndWait(t, resources, seed.name, newConfigMapBody(ns, seed.name, labels))
			}
			query := "filter=metadata.labels." + actions.CaseLabel + "=collection&" + test.query
			actions.WaitForCollection(t, resources, query, test.want)
		})
	}
}
