package render

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	helmchart "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	"sigs.k8s.io/yaml"

	cyclopsv1alpha1 "github.com/cyclops-ui/cyclops/cyclops-ctrl/api/v1alpha1"
	"github.com/cyclops-ui/cyclops/cyclops-ctrl/internal/models"
	"github.com/cyclops-ui/cyclops/cyclops-ctrl/internal/models/helm"
	"github.com/cyclops-ui/cyclops/cyclops-ctrl/pkg/cluster/k8sclient"
)

// region test harness

type fakeKubernetesClient struct {
	k8sclient.IKubernetesClient
}

func (f *fakeKubernetesClient) VersionInfo() (*version.Info, error) {
	return &version.Info{
		Major:      "1",
		Minor:      "29",
		GitVersion: "v1.29.0",
	}, nil
}

func newTestRenderer() *Renderer {
	return NewRenderer(&fakeKubernetesClient{})
}

// valuesDumpTemplate renders the chart's scoped (coalesced) values as YAML so
// tests can assert on the exact values each chart in the tree receives.
const valuesDumpTemplate = `{{ .Values | toYaml }}`

func newChartTemplate(name string, values map[string]interface{}, deps ...*models.Template) *models.Template {
	return &models.Template{
		Name:   name,
		Values: values,
		Templates: []*helmchart.File{
			{Name: "templates/dump.yaml", Data: []byte(valuesDumpTemplate)},
		},
		HelmChartMetadata: &helm.Metadata{
			Name:       name,
			Version:    "1.0.0",
			APIVersion: "v2",
		},
		Dependencies: deps,
	}
}

func newModule(name string, values map[string]interface{}) cyclopsv1alpha1.Module {
	raw, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}

	return cyclopsv1alpha1.Module{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cyclopsv1alpha1.ModuleSpec{
			Values: apiextensionsv1.JSON{Raw: raw},
		},
	}
}

// renderValuesDocs renders the module and returns the rendered YAML documents
// as maps. Documents are ordered by template file name: dependency charts
// ("<root>/charts/<dep>/...") come before the root chart ("<root>/...").
func renderValuesDocs(t *testing.T, module cyclopsv1alpha1.Module, moduleTemplate *models.Template) []map[string]interface{} {
	t.Helper()

	manifest, err := newTestRenderer().HelmTemplate(module, moduleTemplate)
	require.NoError(t, err)

	docs := make([]map[string]interface{}, 0)
	for _, doc := range strings.Split(manifest, "\n---\n") {
		doc = strings.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}

		var m map[string]interface{}
		require.NoError(t, yaml.Unmarshal([]byte(doc), &m))
		docs = append(docs, m)
	}

	return docs
}

// endregion

func TestHelmTemplate_TopLevelDefaultsOnly(t *testing.T) {
	tmpl := newChartTemplate("root", map[string]interface{}{
		"replicas": 1,
		"image": map[string]interface{}{
			"repository": "nginx",
			"tag":        "latest",
		},
	})

	// module submitted directly (bypassing the UI) with no values at all:
	// chart defaults still apply
	docs := renderValuesDocs(t, newModule("m", map[string]interface{}{}), tmpl)
	require.Len(t, docs, 1)
	assert.EqualValues(t, 1, docs[0]["replicas"])
	assert.Equal(t, map[string]interface{}{
		"repository": "nginx",
		"tag":        "latest",
	}, docs[0]["image"])
}

func TestHelmTemplate_ModuleValuesOverrideDefaults(t *testing.T) {
	tmpl := newChartTemplate("root", map[string]interface{}{
		"replicas": 1,
		"image": map[string]interface{}{
			"repository": "nginx",
			"tag":        "latest",
		},
	})

	docs := renderValuesDocs(t, newModule("m", map[string]interface{}{
		"replicas": 3,
		"image": map[string]interface{}{
			"tag": "1.25",
		},
	}), tmpl)

	require.Len(t, docs, 1)
	assert.EqualValues(t, 3, docs[0]["replicas"])
	assert.Equal(t, map[string]interface{}{
		"repository": "nginx",
		"tag":        "1.25",
	}, docs[0]["image"])
}

func TestHelmTemplate_NestedSubchartValuesOrdering(t *testing.T) {
	grandchild := newChartTemplate("grandchild", map[string]interface{}{
		"gcKey": "gc-default",
	})
	sub := newChartTemplate("sub", map[string]interface{}{
		"config": map[string]interface{}{
			"a": "sub-default",
			"b": "sub-default",
		},
	}, grandchild)
	root := newChartTemplate("root", map[string]interface{}{
		"rootKey": "root-default",
		"sub": map[string]interface{}{
			"config": map[string]interface{}{
				"b": "parent-override",
			},
		},
	}, sub)

	docs := renderValuesDocs(t, newModule("m", map[string]interface{}{
		"sub": map[string]interface{}{
			"config": map[string]interface{}{
				"c": "module-override",
			},
		},
	}), root)

	// grandchild, sub, root (sorted by template file path)
	require.Len(t, docs, 3)

	// subchart scope: own defaults, parent overrides by subchart name and
	// module values merged; module values win
	assert.Equal(t, map[string]interface{}{
		"a": "sub-default",
		"b": "parent-override",
		"c": "module-override",
	}, docs[1]["config"])

	// grandchild defaults are in place
	assert.Equal(t, "gc-default", docs[0]["gcKey"])

	// root scope keeps its own defaults and carries the coalesced subchart scope
	assert.Equal(t, "root-default", docs[2]["rootKey"])
	assert.Equal(t, map[string]interface{}{
		"a": "sub-default",
		"b": "parent-override",
		"c": "module-override",
	}, docs[2]["sub"].(map[string]interface{})["config"])
}

func TestHelmTemplate_Alias(t *testing.T) {
	sub := newChartTemplate("sub", map[string]interface{}{
		"foo": "sub-default",
	})
	root := newChartTemplate("root", map[string]interface{}{
		"alias1": map[string]interface{}{
			"bar": "parent-override",
		},
	}, sub)
	root.HelmChartMetadata.Dependencies = []*helm.Dependency{
		{Name: "sub", Version: "1.0.0", Alias: "alias1"},
	}

	docs := renderValuesDocs(t, newModule("m", map[string]interface{}{
		"alias1": map[string]interface{}{
			"foo": "module-override",
		},
		"sub": map[string]interface{}{
			"foo": "must-not-apply",
		},
	}), root)

	require.Len(t, docs, 2)

	// subchart is exposed under its alias: parent overrides and module
	// values targeting the alias apply
	assert.Equal(t, "module-override", docs[0]["foo"])
	assert.Equal(t, "parent-override", docs[0]["bar"])

	// values targeting the original chart name do not reach the aliased chart
	assert.NotEqual(t, "must-not-apply", docs[0]["foo"])
}

func TestHelmTemplate_GlobalsPropagatedToSubcharts(t *testing.T) {
	sub := newChartTemplate("sub", map[string]interface{}{
		"subKey": "sub-default",
	})
	root := newChartTemplate("root", map[string]interface{}{
		"global": map[string]interface{}{
			"domain": "example.com",
			"shared": map[string]interface{}{
				"a": "global-default",
			},
		},
	}, sub)

	docs := renderValuesDocs(t, newModule("m", map[string]interface{}{
		"global": map[string]interface{}{
			"domain": "module.example.com",
		},
	}), root)

	require.Len(t, docs, 2)

	subGlobal := docs[0]["global"].(map[string]interface{})
	assert.Equal(t, "module.example.com", subGlobal["domain"])
	assert.Equal(t, map[string]interface{}{"a": "global-default"}, subGlobal["shared"])

	rootGlobal := docs[1]["global"].(map[string]interface{})
	assert.Equal(t, "module.example.com", rootGlobal["domain"])
}

func TestHelmTemplate_DependencyConditionFromModuleValues(t *testing.T) {
	sub := newChartTemplate("sub", map[string]interface{}{
		"subKey": "sub-default",
	})
	sub.Condition = "sub.enabled"
	root := newChartTemplate("root", map[string]interface{}{
		"rootKey": "root-default",
	}, sub)

	enabled := renderValuesDocs(t, newModule("m", map[string]interface{}{
		"sub": map[string]interface{}{"enabled": true},
	}), root)
	require.Len(t, enabled, 2)
	assert.Equal(t, "sub-default", enabled[0]["subKey"])

	disabled := renderValuesDocs(t, newModule("m", map[string]interface{}{
		"sub": map[string]interface{}{"enabled": false},
	}), root)
	require.Len(t, disabled, 1)
	assert.Equal(t, "root-default", disabled[0]["rootKey"])
}

func TestHelmTemplate_DependencyConditionSeesChartDefaults(t *testing.T) {
	sub := newChartTemplate("sub", map[string]interface{}{
		"subKey": "sub-default",
	})
	sub.Condition = "sub.enabled"
	root := newChartTemplate("root", map[string]interface{}{
		"sub": map[string]interface{}{
			"enabled": true,
		},
	}, sub)

	// the condition key only exists in the parent chart's default values;
	// the dependency must still be enabled (Helm evaluates conditions after
	// coalescing defaults)
	docs := renderValuesDocs(t, newModule("m", map[string]interface{}{}), root)
	require.Len(t, docs, 2)
	assert.Equal(t, "sub-default", docs[0]["subKey"])
}

func TestHelmTemplate_DisabledChartDefaultsParticipateInConditions(t *testing.T) {
	// depB is disabled by its own condition, but its default values still
	// participate when evaluating depA's condition (Helm evaluates
	// conditions before removing disabled charts)
	depA := newChartTemplate("dep-a", map[string]interface{}{
		"aKey": "a-default",
	})
	depA.Condition = "dep-b.feature.enabled"

	depB := newChartTemplate("dep-b", map[string]interface{}{
		"feature": map[string]interface{}{
			"enabled": true,
		},
	})
	depB.Condition = "dep-b.enabled"

	root := newChartTemplate("root", map[string]interface{}{
		"dep-b": map[string]interface{}{
			"enabled": false,
		},
	}, depA, depB)

	docs := renderValuesDocs(t, newModule("m", map[string]interface{}{}), root)

	// dep-a rendered (its condition read dep-b's defaults), dep-b not rendered
	require.Len(t, docs, 2)
	assert.Equal(t, "a-default", docs[0]["aKey"])
	assert.Contains(t, docs[1], "dep-b")
}

func TestHelmTemplate_NestedDependencyCondition(t *testing.T) {
	grandchild := newChartTemplate("grandchild", map[string]interface{}{
		"gcKey": "gc-default",
	})
	grandchild.Condition = "grandchild.enabled"

	sub := newChartTemplate("sub", map[string]interface{}{
		"grandchild": map[string]interface{}{
			"enabled": false,
		},
	}, grandchild)
	sub.Condition = "sub.enabled"

	root := newChartTemplate("root", map[string]interface{}{
		"sub": map[string]interface{}{
			"enabled": true,
		},
	}, sub)

	// nested condition is evaluated relative to the parent chart scope
	docs := renderValuesDocs(t, newModule("m", map[string]interface{}{}), root)
	require.Len(t, docs, 2) // grandchild disabled, sub + root rendered

	docs = renderValuesDocs(t, newModule("m", map[string]interface{}{
		"sub": map[string]interface{}{
			"grandchild": map[string]interface{}{
				"enabled": true,
			},
		},
	}), root)
	require.Len(t, docs, 3)
	assert.Equal(t, "gc-default", docs[0]["gcKey"])
}

func TestHelmTemplate_ArrayReplacedAsWhole(t *testing.T) {
	tmpl := newChartTemplate("root", map[string]interface{}{
		"list":   []interface{}{"a", "b", "c"},
		"scalar": "default",
	})

	docs := renderValuesDocs(t, newModule("m", map[string]interface{}{
		"list":   []interface{}{"x"},
		"scalar": "override",
	}), tmpl)

	require.Len(t, docs, 1)
	assert.Equal(t, []interface{}{"x"}, docs[0]["list"])
	assert.Equal(t, "override", docs[0]["scalar"])
}

func TestHelmTemplate_ExplicitNullDeletesDefaultKey(t *testing.T) {
	tmpl := newChartTemplate("root", map[string]interface{}{
		"dropme": "default-value",
		"nested": map[string]interface{}{
			"a": "default",
		},
		"keepme": "default",
	})

	docs := renderValuesDocs(t, newModule("m", map[string]interface{}{
		"dropme": nil,
		"nested": nil,
	}), tmpl)

	require.Len(t, docs, 1)
	assert.NotContains(t, docs[0], "dropme")
	assert.NotContains(t, docs[0], "nested")
	assert.Equal(t, "default", docs[0]["keepme"])
}

func TestHelmTemplate_TypeReplacement(t *testing.T) {
	tmpl := newChartTemplate("root", map[string]interface{}{
		"port": map[string]interface{}{
			"number": 80,
		},
		"name": "default-name",
	})

	docs := renderValuesDocs(t, newModule("m", map[string]interface{}{
		// scalar replaces map
		"port": 8080,
		// map replaces scalar
		"name": map[string]interface{}{
			"first": "app",
		},
	}), tmpl)

	require.Len(t, docs, 1)
	assert.EqualValues(t, 8080, docs[0]["port"])
	assert.Equal(t, map[string]interface{}{"first": "app"}, docs[0]["name"])
}

func TestHelmTemplate_DirectSubmissionMatchesExpandedDefaults(t *testing.T) {
	// modules created through the UI may carry the expanded chart defaults
	// in spec.values (from the initial-values endpoint); modules submitted
	// directly carry only explicit overrides. Both must render identically.
	defaults := map[string]interface{}{
		"replicas": 2,
		"image": map[string]interface{}{
			"repository": "nginx",
			"tag":        "latest",
		},
		"list": []interface{}{"a", "b"},
	}
	tmpl := newChartTemplate("root", defaults)

	renderer := newTestRenderer()

	direct, err := renderer.HelmTemplate(newModule("m", map[string]interface{}{}), tmpl)
	require.NoError(t, err)

	expanded, err := renderer.HelmTemplate(newModule("m", map[string]interface{}{
		"replicas": 2,
		"image": map[string]interface{}{
			"repository": "nginx",
			"tag":        "latest",
		},
		"list": []interface{}{"a", "b"},
	}), tmpl)
	require.NoError(t, err)

	assert.Equal(t, direct, expanded)
}

func TestHelmTemplate_DoesNotMutateSharedTemplateOrModule(t *testing.T) {
	tmpl := newChartTemplate("root", map[string]interface{}{
		"replicas": 1,
		"nested": map[string]interface{}{
			"key": "default",
		},
	})

	templateValuesBefore, err := json.Marshal(tmpl.Values)
	require.NoError(t, err)

	module := newModule("m", map[string]interface{}{
		"nested": map[string]interface{}{
			"key": "override",
		},
	})
	moduleValuesBefore := string(module.Spec.Values.Raw)

	renderer := newTestRenderer()
	first, err := renderer.HelmTemplate(module, tmpl)
	require.NoError(t, err)

	// same input renders to the same output, object order included
	second, err := renderer.HelmTemplate(module, tmpl)
	require.NoError(t, err)
	assert.Equal(t, first, second)

	// neither the shared template nor the module values were modified
	templateValuesAfter, err := json.Marshal(tmpl.Values)
	require.NoError(t, err)
	assert.JSONEq(t, string(templateValuesBefore), string(templateValuesAfter))
	assert.Equal(t, moduleValuesBefore, string(module.Spec.Values.Raw))
}

func TestHelmTemplate_ImportValuesDiagnosed(t *testing.T) {
	sub := newChartTemplate("sub", nil)
	root := newChartTemplate("root", nil, sub)
	root.HelmChartMetadata.Dependencies = []*helm.Dependency{
		{
			Name:         "sub",
			Version:      "1.0.0",
			ImportValues: []interface{}{"exports"},
		},
	}

	_, err := newTestRenderer().HelmTemplate(newModule("my-module", map[string]interface{}{}), root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "my-module")
	assert.Contains(t, err.Error(), "root")
	assert.Contains(t, err.Error(), "sub")
	assert.Contains(t, err.Error(), "import-values")
}

// endregion values assembly

// region schema validation

const rootSchema = `{
  "$schema": "http://json-schema.org/schema#",
  "type": "object",
  "properties": {
    "replicas": {"type": "integer"}
  }
}`

func TestHelmTemplate_ParentSchemaViolation(t *testing.T) {
	tmpl := newChartTemplate("root", map[string]interface{}{
		"replicas": 1,
	})
	tmpl.RawSchema = []byte(rootSchema)

	_, err := newTestRenderer().HelmTemplate(newModule("my-module", map[string]interface{}{
		"replicas": "three",
	}), tmpl)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "my-module")
	assert.Contains(t, err.Error(), "root")
	assert.Contains(t, err.Error(), "replicas")
	// the error must not leak configured values
	assert.NotContains(t, err.Error(), "three")
}

func TestHelmTemplate_SubchartSchemaViolation(t *testing.T) {
	sub := newChartTemplate("sub", map[string]interface{}{
		"config": map[string]interface{}{
			"a": "default",
		},
	})
	sub.RawSchema = []byte(`{
	  "type": "object",
	  "properties": {
	    "config": {
	      "type": "object",
	      "properties": {
	        "a": {"type": "string"}
	      }
	    }
	  }
	}`)
	root := newChartTemplate("root", nil, sub)

	_, err := newTestRenderer().HelmTemplate(newModule("my-module", map[string]interface{}{
		"sub": map[string]interface{}{
			"config": map[string]interface{}{
				"a": 5,
			},
		},
	}), root)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "my-module")
	assert.Contains(t, err.Error(), "sub")
	assert.Contains(t, err.Error(), "config.a")
}

func TestHelmTemplate_SubchartSchemaReceivesOnlySubchartScopeAndGlobals(t *testing.T) {
	sub := newChartTemplate("sub", map[string]interface{}{
		"config": map[string]interface{}{
			"a": "default",
		},
	})
	// strict subchart schema: anything but the subchart's own keys and
	// globals must be rejected
	sub.RawSchema = []byte(`{
	  "type": "object",
	  "additionalProperties": false,
	  "properties": {
	    "config": {"type": "object"},
	    "global": {"type": "object"}
	  }
	}`)
	root := newChartTemplate("root", map[string]interface{}{
		"rootOnlyKey": "root-default",
		"global": map[string]interface{}{
			"domain": "example.com",
		},
	}, sub)

	// parent-level keys (rootOnlyKey) must not leak into the subchart
	// validation scope; globals do
	_, err := newTestRenderer().HelmTemplate(newModule("my-module", map[string]interface{}{}), root)
	require.NoError(t, err)
}

func TestHelmTemplate_SubchartSchemaSatisfiedBySubchartDefaults(t *testing.T) {
	sub := newChartTemplate("sub", map[string]interface{}{
		"requiredKey": "from-defaults",
	})
	sub.RawSchema = []byte(`{
	  "type": "object",
	  "required": ["requiredKey"]
	}`)
	root := newChartTemplate("root", nil, sub)

	// the subchart schema is validated against coalesced values, so defaults
	// of the subchart satisfy required fields even with empty module values
	_, err := newTestRenderer().HelmTemplate(newModule("my-module", map[string]interface{}{}), root)
	require.NoError(t, err)
}

func TestHelmTemplate_AliasedSubchartSchemaErrorNamesAlias(t *testing.T) {
	sub := newChartTemplate("sub", nil)
	sub.RawSchema = []byte(`{
	  "type": "object",
	  "properties": {
	    "foo": {"type": "string"}
	  }
	}`)
	root := newChartTemplate("root", nil, sub)
	root.HelmChartMetadata.Dependencies = []*helm.Dependency{
		{Name: "sub", Version: "1.0.0", Alias: "alias1"},
	}

	_, err := newTestRenderer().HelmTemplate(newModule("my-module", map[string]interface{}{
		"alias1": map[string]interface{}{
			"foo": 1,
		},
	}), root)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "alias1")
	assert.Contains(t, err.Error(), "foo")
}

func TestHelmTemplate_SchemaValidationDisabledViaEnv(t *testing.T) {
	t.Setenv(envDisableSchemaValidation, "true")

	tmpl := newChartTemplate("root", map[string]interface{}{
		"replicas": 1,
	})
	tmpl.RawSchema = []byte(rootSchema)

	// invalid per the schema, but validation is switched off
	_, err := newTestRenderer().HelmTemplate(newModule("my-module", map[string]interface{}{
		"replicas": "three",
	}), tmpl)
	require.NoError(t, err)
}

// TestValidateAgainstSchema_RawValuesPanic reproduces the issue behind the
// previously commented-out ValidateAgainstSchema call: validating raw
// (non-coalesced) module values panics when a subchart scope is missing.
// Validation must run on coalesced values.
func TestValidateAgainstSchema_RawValuesPanic(t *testing.T) {
	chart := &helmchart.Chart{
		Metadata: &helmchart.Metadata{Name: "root"},
		Schema:   []byte(`{"type": "object"}`),
	}
	chart.AddDependency(&helmchart.Chart{
		Metadata: &helmchart.Metadata{Name: "sub"},
		Schema:   []byte(`{"type": "object"}`),
	})

	rawModuleValues := chartutil.Values{}
	assert.Panics(t, func() {
		_ = chartutil.ValidateAgainstSchema(chart, rawModuleValues)
	})

	// the renderer's wrapper converts the panic into an error instead of
	// crashing the request/reconcile loop
	assert.Error(t, validateAgainstSchema(chart, rawModuleValues))

	// coalesced values always carry the subchart scope and validate cleanly
	coalesced, err := chartutil.CoalesceValues(chart, rawModuleValues)
	require.NoError(t, err)
	assert.NoError(t, validateAgainstSchema(chart, coalesced))
}

// endregion

// region comparison with the previous renderer behavior

// legacyHelmTemplate replicates the previous renderer: chart.Values empty,
// module values passed through uncoalesced, conditions evaluated against raw
// module values. Used to compare old and new manifests for existing modules.
func legacyHelmTemplate(t *testing.T, module cyclopsv1alpha1.Module, moduleTemplate *models.Template) string {
	t.Helper()

	chart := &helmchart.Chart{
		Raw:       []*helmchart.File{},
		Metadata:  mapMetadata(moduleTemplate.HelmChartMetadata),
		Lock:      &helmchart.Lock{},
		Values:    map[string]interface{}{},
		Schema:    moduleTemplate.RawSchema,
		Files:     moduleTemplate.Files,
		Templates: moduleTemplate.Templates,
	}

	values := make(chartutil.Values)
	require.NoError(t, json.Unmarshal(module.Spec.Values.Raw, &values))

	for _, dependency := range moduleTemplate.Dependencies {
		if !evaluateDependencyCondition(dependency.Condition, values) {
			continue
		}

		chart.AddDependency(&helmchart.Chart{
			Raw:       []*helmchart.File{},
			Metadata:  mapMetadata(dependency.HelmChartMetadata),
			Lock:      &helmchart.Lock{},
			Values:    map[string]interface{}{},
			Schema:    dependency.RawSchema,
			Files:     dependency.Files,
			Templates: dependency.Templates,
		})
	}

	top := make(chartutil.Values)
	top["Values"] = values
	top["Release"] = map[string]interface{}{
		"Name":      module.Name,
		"Namespace": "default",
		"Service":   "Helm",
	}
	top["Capabilities"] = Capabilities{
		KubeVersion: CapabilitiesKubeVersion{
			Version:    "v1.29.0",
			Minor:      "29",
			Major:      "1",
			GitVersion: "v1.29.0",
		},
		HelmVersion: mapCapabilitiesHelmVersion(chartutil.DefaultCapabilities),
	}

	out, err := engine.Render(chart, top)
	require.NoError(t, err)

	filenames := make([]string, 0, len(out))
	for filename := range out {
		filenames = append(filenames, filename)
	}
	sort.Strings(filenames)

	manifest := ""
	for _, filename := range filenames {
		if len(strings.TrimSpace(out[filename])) == 0 {
			continue
		}
		manifest += out[filename]
		manifest += "\n---\n"
	}

	return manifest
}

func TestHelmTemplate_ModuleWithExpandedDefaultsUnchanged(t *testing.T) {
	// existing modules may have the expanded chart defaults stored in
	// spec.values; their override priority must not change and the rendered
	// manifest must stay the same
	tmpl := newChartTemplate("root", map[string]interface{}{
		"replicas": 2,
		"image": map[string]interface{}{
			"repository": "nginx",
			"tag":        "latest",
		},
	})

	module := newModule("m", map[string]interface{}{
		// expanded defaults as stored by the UI...
		"replicas": 2,
		"image": map[string]interface{}{
			"repository": "nginx",
			"tag":        "latest",
		},
		// ...plus an explicit override
		"image2": "sidecar:1.0",
	})

	newManifest, err := newTestRenderer().HelmTemplate(module, tmpl)
	require.NoError(t, err)

	assert.Equal(t, legacyHelmTemplate(t, module, tmpl), newManifest)
}

func TestHelmTemplate_SparseModuleGainsDefaults(t *testing.T) {
	// documents the behavior change for modules submitted directly with
	// sparse values: chart defaults now apply
	tmpl := newChartTemplate("root", map[string]interface{}{
		"replicas": 2,
	})

	module := newModule("m", map[string]interface{}{})

	newManifest, err := newTestRenderer().HelmTemplate(module, tmpl)
	require.NoError(t, err)

	legacyManifest := legacyHelmTemplate(t, module, tmpl)

	assert.NotEqual(t, legacyManifest, newManifest)
	assert.Contains(t, newManifest, "replicas: 2")
	assert.NotContains(t, legacyManifest, "replicas")
}

func TestHelmTemplate_ConditionDefaultsChangeEnablement(t *testing.T) {
	// documents the behavior change for conditions: a dependency enabled by
	// chart defaults (condition key absent from module values) previously
	// did not render and now does
	sub := newChartTemplate("sub", map[string]interface{}{
		"subKey": "sub-default",
	})
	sub.Condition = "sub.enabled"
	root := newChartTemplate("root", map[string]interface{}{
		"sub": map[string]interface{}{
			"enabled": true,
		},
	}, sub)

	module := newModule("m", map[string]interface{}{})

	newManifest, err := newTestRenderer().HelmTemplate(module, root)
	require.NoError(t, err)
	legacyManifest := legacyHelmTemplate(t, module, root)

	assert.Contains(t, newManifest, "subKey: sub-default")
	assert.NotContains(t, legacyManifest, "subKey")
}

// endregion
