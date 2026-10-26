package config

import "phenix/store"

type DataType int

const (
	DataTypeUnknown DataType = iota
	DataTypeJSON
	DataTypeYAML
)

type CreateOption func(*createOptions)

type createOptions struct {
	config   *store.Config
	path     string
	data     []byte
	dataType DataType
	validate bool
}

func newCreateOptions(opts ...CreateOption) createOptions {
	var o createOptions

	for _, opt := range opts {
		opt(&o)
	}

	return o
}

func CreateFromConfig(c *store.Config) CreateOption {
	return func(o *createOptions) {
		o.config = c
	}
}

func CreateFromPath(p string) CreateOption {
	return func(o *createOptions) {
		o.path = p
	}
}

func CreateFromJSON(d []byte) CreateOption {
	return func(o *createOptions) {
		o.data = d
		o.dataType = DataTypeJSON
	}
}

func CreateFromYAML(d []byte) CreateOption {
	return func(o *createOptions) {
		o.data = d
		o.dataType = DataTypeYAML
	}
}

func CreateWithValidation() CreateOption {
	return func(o *createOptions) {
		o.validate = true
	}
}

type UpdateOption func(*updateOptions)

type updateOptions struct {
	force bool
}

func newUpdateOptions(opts ...UpdateOption) updateOptions {
	var o updateOptions

	for _, opt := range opts {
		opt(&o)
	}

	return o
}

// UpdateWithForce lets [Update] replace the config of a running experiment,
// with the status the given config carries, as phenix config edit --force
// does. Without it, Update refuses one. Update never renames a running
// experiment.
func UpdateWithForce(force bool) UpdateOption {
	return func(o *updateOptions) {
		o.force = force
	}
}
