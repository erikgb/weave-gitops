package server

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/weaveworks/weave-gitops/core/server/types"
)

type matchLabelOptionFn func() (key, value string)

func matchLabel(options ...matchLabelOptionFn) client.MatchingLabels {
	opts := map[string]string{}

	for _, fn := range options {
		key, value := fn()

		if key != "" && value != "" {
			opts[key] = value
		}
	}

	return opts
}

func withPartOfLabel(name string) matchLabelOptionFn {
	return func() (string, string) {
		if name != "" {
			return types.PartOfLabel, name
		}

		return "", ""
	}
}
