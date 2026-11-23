package render

import (
	"fmt"
	"os"
	"sort"
	"strings"

	json "github.com/json-iterator/go"
	helmchart "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"

	cyclopsv1alpha1 "github.com/cyclops-ui/cyclops/cyclops-ctrl/api/v1alpha1"
	"github.com/cyclops-ui/cyclops/cyclops-ctrl/internal/models"
	"github.com/cyclops-ui/cyclops/cyclops-ctrl/internal/models/helm"
	"github.com/cyclops-ui/cyclops/cyclops-ctrl/pkg/cluster/k8sclient"
)

// envDisableSchemaValidation is an escape hatch that disables JSON schema
// validation of the coalesced values when set to "true". Values assembly
// (chart defaults, dependency defaults, globals) is not affected by it.
const envDisableSchemaValidation = "CYCLOPS_DISABLE_VALUES_SCHEMA_VALIDATION"

type Renderer struct {
	k8sClient k8sclient.IKubernetesClient
}

func NewRenderer(kubernetesClient k8sclient.IKubernetesClient) *Renderer {
	return &Renderer{
		k8sClient: kubernetesClient,
	}
}

func (r *Renderer) HelmTemplate(module cyclopsv1alpha1.Module, moduleTemplate *models.Template) (string, error) {
	if moduleTemplate == nil {
		return "", nil
	}

	values := make(chartutil.Values)
	if err := json.Unmarshal(module.Spec.Values.Raw, &values); err != nil {
		return "", err
	}

	if err := checkImportValues(moduleTemplate); err != nil {
		return "", fmt.Errorf("module %v: %w", module.Name, err)
	}

	chart, err := mapChart(moduleTemplate)
	if err != nil {
		return "", fmt.Errorf("module %v: %w", module.Name, err)
	}

	// Dependency conditions are evaluated against coalesced values (chart
	// defaults included), mirroring Helm's dependency processing stage where
	// conditions are checked after coalescing and before disabled charts are
	// removed. Defaults of charts that end up disabled therefore still
	// participate in condition evaluation.
	if err := applyDependencyConditions(chart, moduleTemplate, values); err != nil {
		return "", fmt.Errorf("module %v: %w", module.Name, err)
	}

	// Assemble the final values following Helm's value processing
	// conventions: module values take precedence over chart defaults, maps
	// are merged recursively, arrays and scalars are replaced as a whole,
	// explicit nulls delete the default key, and globals are propagated
	// into each subchart's scope.
	coalescedValues, err := chartutil.CoalesceValues(chart, values)
	if err != nil {
		return "", fmt.Errorf("module %v: failed to assemble helm values: %w", module.Name, err)
	}

	if err := validateValues(module.Name, chart, coalescedValues); err != nil {
		return "", err
	}

	top := make(chartutil.Values)
	top["Values"] = coalescedValues
	top["Release"] = map[string]interface{}{
		"Name":      module.Name,
		"Namespace": mapTargetNamespace(module.Spec.TargetNamespace),
		"Service":   "Helm",
	}

	versionInfo, err := r.k8sClient.VersionInfo()
	if err != nil {
		return "", err
	}

	defaultCapabilites := chartutil.DefaultCapabilities
	top["Capabilities"] = Capabilities{
		KubeVersion: CapabilitiesKubeVersion{
			Version:    versionInfo.String(),
			Minor:      versionInfo.Minor,
			Major:      versionInfo.Major,
			GitVersion: versionInfo.GitVersion,
		},
		HelmVersion: mapCapabilitiesHelmVersion(defaultCapabilites),
	}

	out, err := engine.Render(chart, top)
	if err != nil {
		return "", err
	}

	filenames := make([]string, 0, len(out))
	for filename := range out {
		filenames = append(filenames, filename)
	}

	sort.Strings(filenames)

	manifest := ""
	for _, filename := range filenames {
		renderedManifest := out[filename]

		if len(strings.TrimSpace(renderedManifest)) == 0 {
			continue
		}

		manifest += renderedManifest
		manifest += "\n---\n"
	}

	return manifest, err
}

// mapChart builds a helm chart tree from the given template. Chart default
// values come from each chart's values.yaml (stored on the template at load
// time). Aliased dependencies are exposed under their alias, as Helm does
// when processing dependencies: parent values targeting the alias apply to
// the dependency, values targeting the original chart name do not.
func mapChart(moduleTemplate *models.Template) (*helmchart.Chart, error) {
	values := moduleTemplate.Values
	if values == nil {
		values = map[string]interface{}{}
	}

	chart := &helmchart.Chart{
		Raw:       []*helmchart.File{},
		Metadata:  mapMetadata(moduleTemplate.HelmChartMetadata),
		Lock:      &helmchart.Lock{},
		Values:    values,
		Schema:    moduleTemplate.RawSchema,
		Files:     moduleTemplate.Files,
		Templates: moduleTemplate.Templates,
	}

	for _, dependency := range moduleTemplate.Dependencies {
		dependencyChart, err := mapChart(dependency)
		if err != nil {
			return nil, err
		}

		if alias := dependencyAlias(moduleTemplate.HelmChartMetadata, dependency.Name); len(alias) > 0 {
			dependencyChart.Metadata.Name = alias
		}

		chart.AddDependency(dependencyChart)
	}

	return chart, nil
}

// checkImportValues diagnoses charts relying on import-values instead of
// silently ignoring them. Importing values from child to parent charts is
// currently not supported.
func checkImportValues(moduleTemplate *models.Template) error {
	if moduleTemplate.HelmChartMetadata != nil {
		for _, dependency := range moduleTemplate.HelmChartMetadata.Dependencies {
			if dependency == nil || len(dependency.ImportValues) == 0 {
				continue
			}

			return fmt.Errorf(
				"chart %v dependency %v declares import-values which is not supported",
				chartName(moduleTemplate),
				dependency.Name,
			)
		}
	}

	for _, dependency := range moduleTemplate.Dependencies {
		if err := checkImportValues(dependency); err != nil {
			return err
		}
	}

	return nil
}

func dependencyAlias(metadata *helm.Metadata, dependencyName string) string {
	if metadata == nil {
		return ""
	}

	for _, dependency := range metadata.Dependencies {
		if dependency != nil && dependency.Name == dependencyName && len(dependency.Alias) > 0 {
			return dependency.Alias
		}
	}

	return ""
}

// applyDependencyConditions removes charts disabled by their condition from
// the chart tree. Conditions are evaluated against values coalesced with all
// charts still present, so defaults of disabled charts keep their meaning in
// condition expressions.
func applyDependencyConditions(chart *helmchart.Chart, moduleTemplate *models.Template, values chartutil.Values) error {
	coalesced, err := chartutil.CoalesceValues(chart, values)
	if err != nil {
		return fmt.Errorf("failed to evaluate dependency conditions: %w", err)
	}

	removeDisabledDependencies(chart, moduleTemplate, coalesced, "")

	return nil
}

func removeDisabledDependencies(
	chart *helmchart.Chart,
	moduleTemplate *models.Template,
	coalesced chartutil.Values,
	path string,
) {
	dependencies := chart.Dependencies()
	enabled := make([]*helmchart.Chart, 0, len(dependencies))

	for i, dependencyTemplate := range moduleTemplate.Dependencies {
		dependencyChart := dependencies[i]

		if len(dependencyTemplate.Condition) > 0 &&
			!evaluateDependencyCondition(path+dependencyTemplate.Condition, coalesced) {
			continue
		}

		removeDisabledDependencies(
			dependencyChart,
			dependencyTemplate,
			coalesced,
			path+dependencyChart.Metadata.Name+".",
		)

		enabled = append(enabled, dependencyChart)
	}

	chart.SetDependencies(enabled...)
}

// validateValues validates the coalesced values against the JSON schemas of
// the chart and each of its (enabled) dependencies. The top-level schema
// receives the top-level values; each subchart schema receives only that
// subchart's scope (globals included). The returned error names the module,
// the chart (or alias) and the offending field path, but never the
// configured values.
func validateValues(moduleName string, chart *helmchart.Chart, values chartutil.Values) error {
	if os.Getenv(envDisableSchemaValidation) == "true" {
		return nil
	}

	if err := validateAgainstSchema(chart, values); err != nil {
		return fmt.Errorf(
			"module %v: values don't meet the specifications of the schema(s) in the following chart(s):\n%v",
			moduleName,
			err,
		)
	}

	return nil
}

// validateAgainstSchema wraps chartutil.ValidateAgainstSchema and converts
// panics into errors so a faulty chart can not crash the request or the
// reconcile loop; the failure is surfaced as a render error instead.
func validateAgainstSchema(chart *helmchart.Chart, values chartutil.Values) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("failed to validate values against schema: %v", r)
		}
	}()

	return chartutil.ValidateAgainstSchema(chart, values)
}

func chartName(moduleTemplate *models.Template) string {
	if moduleTemplate.HelmChartMetadata != nil && len(moduleTemplate.HelmChartMetadata.Name) > 0 {
		return moduleTemplate.HelmChartMetadata.Name
	}

	return moduleTemplate.Name
}

func mapMetadata(metadata *helm.Metadata) *helmchart.Metadata {
	if metadata == nil {
		return &helmchart.Metadata{}
	}

	dependencies := make([]*helmchart.Dependency, 0, len(metadata.Dependencies))
	for _, dependency := range metadata.Dependencies {
		dependencies = append(dependencies, &helmchart.Dependency{
			Name:         dependency.Name,
			Version:      dependency.Version,
			Repository:   dependency.Repository,
			Condition:    dependency.Condition,
			Tags:         dependency.Tags,
			Enabled:      dependency.Enabled,
			ImportValues: dependency.ImportValues,
			Alias:        dependency.Alias,
		})
	}

	maintainers := make([]*helmchart.Maintainer, 0, len(metadata.Maintainers))
	for _, maintainer := range metadata.Maintainers {
		maintainers = append(maintainers, &helmchart.Maintainer{
			Name:  maintainer.Name,
			Email: maintainer.Email,
			URL:   maintainer.URL,
		})
	}

	return &helmchart.Metadata{
		Name:         metadata.Name,
		Home:         metadata.Home,
		Sources:      metadata.Sources,
		Version:      metadata.Version,
		Description:  metadata.Description,
		Keywords:     metadata.Keywords,
		Maintainers:  maintainers,
		Icon:         metadata.Icon,
		APIVersion:   metadata.APIVersion,
		Condition:    metadata.Condition,
		Tags:         metadata.Tags,
		AppVersion:   metadata.AppVersion,
		Deprecated:   metadata.Deprecated,
		Annotations:  metadata.Annotations,
		KubeVersion:  metadata.KubeVersion,
		Dependencies: dependencies,
		Type:         metadata.Type,
	}
}

func evaluateDependencyCondition(condition string, values map[string]interface{}) bool {
	if len(condition) == 0 {
		return true
	}

	keys := strings.Split(condition, ".")
	var current interface{} = values

	for _, key := range keys {
		if m, ok := current.(map[string]interface{}); ok {
			if val, exists := m[key]; exists {
				current = val
			} else {
				return false
			}
		} else {
			return false
		}
	}

	if result, ok := current.(bool); ok {
		return result
	}

	return false
}

func mapTargetNamespace(namespace string) string {
	if len(namespace) == 0 {
		return "default"
	}

	return namespace
}

func mapCapabilitiesHelmVersion(defaultCapabilities *chartutil.Capabilities) CapabilitiesHelmVersion {
	if defaultCapabilities == nil {
		return CapabilitiesHelmVersion{}
	}

	return CapabilitiesHelmVersion{
		Version:      defaultCapabilities.HelmVersion.Version,
		GitCommit:    defaultCapabilities.HelmVersion.GitCommit,
		GitTreeState: defaultCapabilities.HelmVersion.GitTreeState,
		GoVersion:    defaultCapabilities.HelmVersion.GoVersion,
	}
}

type CapabilitiesKubeVersion struct {
	Version    string
	Minor      string
	Major      string
	GitVersion string
}

type CapabilitiesHelmVersion struct {
	Version      string `json:"version,omitempty"`
	GitCommit    string `json:"git_commit,omitempty"`
	GitTreeState string `json:"git_tree_state,omitempty"`
	GoVersion    string `json:"go_version,omitempty"`
}

type Capabilities struct {
	APIVersions chartutil.VersionSet
	KubeVersion CapabilitiesKubeVersion
	HelmVersion CapabilitiesHelmVersion
}
