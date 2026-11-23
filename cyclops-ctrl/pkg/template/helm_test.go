package template

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMapHelmChart_ExtractsDefaultValues(t *testing.T) {
	r := Repo{}

	files := map[string][]byte{
		"demo/Chart.yaml":                   []byte("apiVersion: v2\nname: demo\nversion: 1.0.0\n"),
		"demo/values.yaml":                  []byte("replicas: 2\nimage:\n  tag: latest\n"),
		"demo/templates/cm.yaml":            []byte("apiVersion: v1\nkind: ConfigMap\n"),
		"demo/charts/sub/Chart.yaml":        []byte("apiVersion: v2\nname: sub\nversion: 1.0.0\n"),
		"demo/charts/sub/values.yaml":       []byte("config:\n  a: 1\n"),
		"demo/charts/sub/templates/cm.yaml": []byte("apiVersion: v1\nkind: Secret\n"),
	}

	tmpl, err := r.mapHelmChart("demo", files)
	require.NoError(t, err)

	// chart defaults are captured on the template for render-time assembly
	assert.Equal(t, map[string]interface{}{
		"replicas": float64(2),
		"image": map[string]interface{}{
			"tag": "latest",
		},
	}, tmpl.Values)

	// values.yaml is not exposed through .Files (matching Helm's loader)
	for _, f := range tmpl.Files {
		assert.NotContains(t, f.Name, "values.yaml")
	}

	// dependency chart defaults are captured as well
	require.Len(t, tmpl.Dependencies, 1)
	assert.Equal(t, map[string]interface{}{
		"config": map[string]interface{}{
			"a": float64(1),
		},
	}, tmpl.Dependencies[0].Values)
}

func TestMapHelmChart_InvalidValuesYAML(t *testing.T) {
	r := Repo{}

	files := map[string][]byte{
		"demo/Chart.yaml":  []byte("apiVersion: v2\nname: demo\nversion: 1.0.0\n"),
		"demo/values.yaml": []byte("replicas: [unclosed"),
	}

	_, err := r.mapHelmChart("demo", files)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "values.yaml")
}
