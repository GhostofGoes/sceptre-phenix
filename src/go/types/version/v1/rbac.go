package v1

type RoleSpec struct {
	Name           string          `json:"roleName"       mapstructure:"roleName"       structs:"roleName"       yaml:"roleNname"`
	Policies       []*PolicySpec   `json:"policies"       mapstructure:"policies"       structs:"policies"       yaml:"policies"`
	ResourceLimits *ResourceLimits `json:"resourceLimits" mapstructure:"resourceLimits" structs:"resourceLimits" yaml:"resourceLimits,omitempty"`
}

// ResourceLimits caps how much of a resource a role's users may consume per
// VM or per experiment. A zero/unset field means "no limit". These are
// enforced independently of the resource/verb/resourceName policies above.
type ResourceLimits struct {
	// MaxVCPUs caps the number of vCPUs a single VM may be given.
	MaxVCPUs int `json:"maxVCPUs" mapstructure:"maxVCPUs" structs:"maxVCPUs" yaml:"maxVCPUs,omitempty"`
	// MaxMemoryMB caps the amount of RAM (in MB) a single VM may be given.
	MaxMemoryMB int `json:"maxMemoryMB" mapstructure:"maxMemoryMB" structs:"maxMemoryMB" yaml:"maxMemoryMB,omitempty"`
	// MaxDiskGB caps the size (in GB) a disk image may be resized to.
	MaxDiskGB int `json:"maxDiskGB" mapstructure:"maxDiskGB" structs:"maxDiskGB" yaml:"maxDiskGB,omitempty"`
	// MaxVMsPerExperiment caps the number of VMs allowed in a single topology.
	MaxVMsPerExperiment int `json:"maxVMsPerExperiment" mapstructure:"maxVMsPerExperiment" structs:"maxVMsPerExperiment" yaml:"maxVMsPerExperiment,omitempty"` //nolint:lll // struct tags
}

type PolicySpec struct {
	Resources     []string `json:"resources"     mapstructure:"resources"     structs:"resources"     yaml:"resources"`
	ResourceNames []string `json:"resourceNames" mapstructure:"resourceNames" structs:"resourceNames" yaml:"resourceNames"`
	Verbs         []string `json:"verbs"         mapstructure:"verbs"         structs:"verbs"         yaml:"verbs"`
}
