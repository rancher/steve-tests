package query

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestListOptionsValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options ListOptions
		want    url.Values
		wantErr string
	}{
		{
			name:    "empty options",
			options: ListOptions{},
			want:    url.Values{},
		},
		{
			name: "repeated filters stay separate",
			options: ListOptions{Filters: []string{
				Equal("metadata.namespace", "qa one"),
				NotEqual("metadata.labels[tier]", "slow"),
			}},
			want: url.Values{FilterParam: {
				"metadata.namespace=qa one",
				"metadata.labels[tier]!=slow",
			}},
		},
		{
			name: "sort pagination and revision",
			options: ListOptions{
				Sort:     []string{"metadata.namespace", "-metadata.name"},
				Page:     3,
				PageSize: 25,
				Revision: "1042",
			},
			want: url.Values{
				SortParam:     {"metadata.namespace,-metadata.name"},
				PageParam:     {"3"},
				PageSizeParam: {"25"},
				RevisionParam: {"1042"},
			},
		},
		{
			name: "Kubernetes chunk limit and continue token",
			options: ListOptions{
				Limit:    250,
				Continue: "opaque+/=token",
			},
			want: url.Values{
				LimitParam:    {"250"},
				ContinueParam: {"opaque+/=token"},
			},
		},
		{
			name:    "unlimited Kubernetes chunk",
			options: ListOptions{Limit: -1},
			want:    url.Values{LimitParam: {"-1"}},
		},
		{
			name: "projects or namespaces include",
			options: ListOptions{
				ProjectsOrNamespaces: []string{"project:cattle-system", "qa"},
			},
			want: url.Values{ProjectsOrNamespacesParam: {"project:cattle-system,qa"}},
		},
		{
			name: "projects or namespaces exclude",
			options: ListOptions{
				ExcludeProjectsOrNamespaces: []string{"kube-system", "cattle-system"},
			},
			want: url.Values{ProjectsOrNamespacesParam + "!": {"kube-system,cattle-system"}},
		},
		{
			name: "summary controls",
			options: ListOptions{
				Summary:           []string{"metadata.state.name", "metadata.namespace"},
				SummaryOnly:       Bool(true),
				SummaryNamespaced: Bool(false),
			},
			want: url.Values{
				SummaryParam:           {"metadata.state.name,metadata.namespace"},
				SummaryOnlyParam:       {"true"},
				SummaryNamespacedParam: {"false"},
			},
		},
		{
			name: "associated data explicitly disabled",
			options: ListOptions{
				IncludeAssociatedData: Bool(false),
			},
			want: url.Values{IncludeAssociatedDataParam: {"false"}},
		},
		{
			name:    "negative page",
			options: ListOptions{Page: -1},
			wantErr: "page must not be negative",
		},
		{
			name:    "negative page size",
			options: ListOptions{PageSize: -1},
			wantErr: "pagesize must not be negative",
		},
		{
			name:    "invalid chunk limit",
			options: ListOptions{Limit: -2},
			wantErr: "limit must be -1",
		},
		{
			name: "include and exclude conflict",
			options: ListOptions{
				ProjectsOrNamespaces:        []string{"one"},
				ExcludeProjectsOrNamespaces: []string{"two"},
			},
			wantErr: "include and exclude cannot be combined",
		},
		{
			name:    "summaryonly needs summary",
			options: ListOptions{SummaryOnly: Bool(true)},
			wantErr: "summaryonly requires",
		},
		{
			name:    "summarynamespaced needs summary",
			options: ListOptions{SummaryNamespaced: Bool(true)},
			wantErr: "summarynamespaced requires",
		},
		{
			name:    "empty filter",
			options: ListOptions{Filters: []string{"  "}},
			wantErr: "filter expression must not be empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := test.options.Values()
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Values() error = %v, want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Values() returned error: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Values() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestListOptionsEncodeEscapesSteveGrammar(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, value, sort string }{
		{name: "image with spaces", value: "registry.example/qa image:v1", sort: "metadata.labels[kubernetes.io/arch]"},
		{name: "query delimiters", value: "a+b&c=d", sort: "metadata.name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filter := Contains("spec.containers.image", test.value)
			encoded, err := (ListOptions{Filters: []string{filter}, Sort: []string{test.sort}}).Encode()
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.ParseQuery(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if got := parsed.Get(FilterParam); got != filter {
				t.Fatalf("decoded filter = %q, want %q", got, filter)
			}
			if got := parsed.Get(SortParam); got != test.sort {
				t.Fatalf("decoded sort = %q, want %q", got, test.sort)
			}
		})
	}
}

func TestListOptionsEncodeRejectsInvalidOptionsWithoutPartialQuery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		options ListOptions
		wantErr string
	}{
		{
			name:    "valid filter followed by blank expression",
			options: ListOptions{Filters: []string{"metadata.name=example", "\t\n\u00a0"}},
			wantErr: "filter expression must not be empty",
		},
		{
			name:    "explicit false summaryonly still requires fields",
			options: ListOptions{SummaryOnly: Bool(false)},
			wantErr: "summaryonly requires",
		},
		{
			name:    "explicit false summarynamespaced still requires fields",
			options: ListOptions{SummaryNamespaced: Bool(false)},
			wantErr: "summarynamespaced requires",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := test.options.Encode()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Encode() error = %v, want substring %q", err, test.wantErr)
			}
			if encoded != "" {
				t.Fatalf("Encode() returned partial query %q for invalid options", encoded)
			}
		})
	}
}

func TestListOptionsEncodePreservesOpaqueTokensAndRepeatedFilters(t *testing.T) {
	t.Parallel()
	filters := []string{
		Contains("metadata.annotations[note]", "quote\" and slash\\\nnext line"),
		Equal("metadata.name", "a+b&c=d"),
	}
	options := ListOptions{
		Filters:  filters,
		Revision: "revision+with/&?=delimiters%20",
		Continue: "opaque+/==&filter=metadata.name%3Dother#fragment",
	}
	encoded, err := options.Encode()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.ParseQuery(encoded)
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{
		FilterParam:   filters,
		RevisionParam: {options.Revision},
		ContinueParam: {options.Continue},
	}
	if !reflect.DeepEqual(parsed, want) {
		t.Fatalf("decoded query = %#v, want %#v", parsed, want)
	}
}

func TestFilterExpressionHelpers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "equal", got: Equal("metadata.name", "alpha"), want: "metadata.name=alpha"},
		{name: "not equal", got: NotEqual("metadata.name", "alpha"), want: "metadata.name!=alpha"},
		{name: "contains", got: Contains("spec.values", "a b"), want: `spec.values CONTAINS "a b"`},
		{name: "not contains", got: NotContains("spec.values", "a b"), want: `spec.values NOTCONTAINS "a b"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.got != test.want {
				t.Fatalf("expression = %q, want %q", test.got, test.want)
			}
		})
	}
}

func TestEqualityHelpersStayUnquotedForNonSQLCompatibility(t *testing.T) {
	t.Parallel()
	value := `alpha beta,quote'and\backslash`
	tests := []struct {
		name     string
		build    func(string, string) string
		operator string
	}{
		{name: "equal", build: Equal, operator: "="},
		{name: "not equal", build: NotEqual, operator: "!="},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want := "metadata.name" + test.operator + value
			if got := test.build("metadata.name", value); got != want {
				t.Fatalf("expression = %q, want %q", got, want)
			}
		})
	}
}

func TestSQLSetMembershipHelpersValidateAndCanonicalizeValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, field string
		build       func(string, ...string) (string, error)
		values      []string
		want        string
	}{
		{name: "in sorts and deduplicates", field: "metadata.name", build: In,
			values: []string{"zulu", "registry.example.test/app:v1", "alpha-value", "_reserved", "zulu"},
			want:   "metadata.name in (_reserved,alpha-value,registry.example.test/app:v1,zulu)"},
		{name: "notin sorts", field: "metadata.labels[app.kubernetes.io/name]", build: NotIn,
			values: []string{"beta", "alpha"}, want: "metadata.labels[app.kubernetes.io/name] notin (alpha,beta)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := append([]string(nil), test.values...)
			got, err := test.build(test.field, test.values...)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("expression = %q, want %q", got, test.want)
			}
			if !reflect.DeepEqual(test.values, original) {
				t.Fatalf("helper mutated input: %v", test.values)
			}
			encoded, err := (ListOptions{Filters: []string{got}}).Encode()
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.ParseQuery(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Get(FilterParam) != got {
				t.Fatalf("filter round trip = %q, want %q", parsed.Get(FilterParam), got)
			}
		})
	}
}

func TestSQLSetMembershipHelpersRejectInvalidGrammar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		build   func() (string, error)
		wantErr string
	}{
		{name: "empty field", build: func() (string, error) { return In("", "value") }, wantErr: "field must not be empty"},
		{name: "field injection", build: func() (string, error) {
			return In("metadata.name,metadata.namespace", "value")
		}, wantErr: "invalid grammar character"},
		{name: "empty set", build: func() (string, error) { return NotIn("metadata.name") }, wantErr: "at least one value"},
		{name: "empty value", build: func() (string, error) { return In("metadata.name", "") }, wantErr: "must not be empty"},
		{name: "leading dash", build: func() (string, error) { return In("metadata.name", "-value") }, wantErr: "must start"},
		{name: "space value", build: func() (string, error) { return In("metadata.name", "space value") }, wantErr: "unsupported Steve set-grammar"},
		{name: "comma value", build: func() (string, error) { return In("metadata.name", "comma,value") }, wantErr: "unsupported Steve set-grammar"},
		{name: "quoted value", build: func() (string, error) { return In("metadata.name", `quote"value`) }, wantErr: "unsupported Steve set-grammar"},
		{name: "backslash value", build: func() (string, error) { return In("metadata.name", `slash\value`) }, wantErr: "unsupported Steve set-grammar"},
		{name: "NUL value", build: func() (string, error) { return In("metadata.name", "bad\x00value") }, wantErr: "unsupported Steve set-grammar"},
		{name: "Unicode value", build: func() (string, error) { return In("metadata.name", "café") }, wantErr: "ASCII"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := test.build(); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("helper error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}
