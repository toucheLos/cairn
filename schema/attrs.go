package schema

import "sort"

// AttrSpec describes one registered detail key.
type AttrSpec struct {
	// Doc is a one-line description of what the value holds.
	Doc string

	// PII marks a value that can carry identifying information — a hostname, a
	// username, a path under /home, an account code.
	//
	// This flag is what lets the redaction layer operate at the boundary rather
	// than at the call site (CLAUDE.md §10). The redactor pseudonymizes every
	// value whose key is marked here without needing to know which collector
	// produced it. Marking a key that turns out to be safe costs a slightly
	// noisier bundle; failing to mark one that is not costs an IP incident, so
	// the registry errs toward marking.
	PII bool
}

// universalAttrs are permitted on any class. Kept deliberately short: a key that
// is meaningful for every class is usually a key that is meaningful for none.
var universalAttrs = map[string]AttrSpec{
	"exit_code": {Doc: "process or job exit status as reported by the producer"},
	"signal":    {Doc: "terminating signal name, e.g. SIGKILL"},
	"state":     {Doc: "producer-reported state string, e.g. OUT_OF_MEMORY, DOWN"},
	"severity":  {Doc: "producer-reported severity: info, warn, error"},
	"user":      {Doc: "owning local account name", PII: true},
	"account":   {Doc: "Slurm account, project, or allocation code", PII: true},
	"partition": {Doc: "scheduler partition or queue name", PII: true},
}

// classAttrs registers the keys each class may carry, beyond universalAttrs.
//
// An unregistered key is a validation error, not a warning. This registry is the
// mechanism behind CLAUDE.md §4's requirement that detail be "bounded and
// structured; not a dumping ground for raw log lines" — without it, the bound is
// a comment, and the first collector under deadline pressure breaks it.
//
// Adding a key here is an ordinary additive change. Adding one that carries free
// text is not: prefer extracting the field you actually need.
var classAttrs = map[Class]map[string]AttrSpec{
	ClassUnknown: {},

	ClassAppNonzeroExit: {
		"command": {Doc: "executable name, without arguments or path", PII: true},
	},
	ClassAppSegfault: {
		"comm":       {Doc: "kernel-reported command name of the faulting process", PII: true},
		"pid":        {Doc: "faulting process id"},
		"fault_addr": {Doc: "faulting address as reported by the kernel"},
	},

	ClassResourceOOM: {
		"cgroup":      {Doc: "cgroup path in which the kill occurred", PII: true},
		"limit_bytes": {Doc: "memory limit at the time of the kill"},
		"usage_bytes": {Doc: "memory in use at the time of the kill"},
		"killed_pid":  {Doc: "pid selected by the OOM killer"},
		"killed_comm": {Doc: "command name of the killed process", PII: true},
	},
	ClassResourceOOMGPU: {
		"gpu_index":       {Doc: "device index on the node"},
		"limit_bytes":     {Doc: "device memory capacity"},
		"usage_bytes":     {Doc: "device memory in use"},
		"requested_bytes": {Doc: "size of the allocation that failed"},
	},
	ClassResourceWalltimeExceeded: {
		"limit_seconds":   {Doc: "wall clock limit of the job or partition"},
		"elapsed_seconds": {Doc: "wall clock consumed at termination"},
	},
	ClassResourceDiskQuota: {
		"mount":       {Doc: "mount point at which the quota was hit", PII: true},
		"fs_type":     {Doc: "filesystem type, e.g. lustre, gpfs, nfs"},
		"limit_bytes": {Doc: "quota limit"},
		"usage_bytes": {Doc: "usage at the time of the failure"},
	},

	ClassSchedNodeFail: {
		"reason": {Doc: "scheduler-reported reason string"},
	},
	ClassSchedPreempted: {
		"reason": {Doc: "scheduler-reported reason string"},
		"by":     {Doc: "job id that triggered the preemption"},
	},
	ClassSchedCancelled: {
		"reason": {Doc: "scheduler-reported reason string"},
		"by":     {Doc: "account that issued the cancellation", PII: true},
	},
	ClassSchedRequeued: {
		"reason": {Doc: "scheduler-reported reason string"},
	},

	ClassAuthMunge: {
		"reason":   {Doc: "munge error string, e.g. Expired credential"},
		"peer":     {Doc: "peer host of the failed exchange", PII: true},
		"skew_sec": {Doc: "observed clock skew in seconds, when reported"},
		"daemon":   {Doc: "daemon that reported the failure, e.g. slurmd"},
	},
	ClassAuthCredential: {
		"reason":    {Doc: "producer-reported failure reason"},
		"cred_type": {Doc: "credential kind, e.g. kerberos, token, ssh"},
	},

	ClassFabricLinkFlap: {
		"device":     {Doc: "HCA device name, e.g. mlx5_0"},
		"port":       {Doc: "port number on the device"},
		"link_state": {Doc: "logical link state, e.g. Active, Down"},
		"phys_state": {Doc: "physical port state, e.g. LinkUp, Polling"},
		"rate":       {Doc: "negotiated rate as reported, e.g. 200 Gb/sec"},
		"flap_count": {Doc: "transitions observed within the collection window"},
	},
	ClassFabricCongestion: {
		"device":        {Doc: "HCA device name"},
		"port":          {Doc: "port number on the device"},
		"counter":       {Doc: "performance counter name, e.g. PortXmitWait"},
		"counter_delta": {Doc: "counter increase across the collection window"},
	},
	ClassFabricNCCLTimeout: {
		"rank":            {Doc: "NCCL rank reporting the timeout"},
		"world_size":      {Doc: "communicator size"},
		"op":              {Doc: "collective operation, e.g. AllReduce"},
		"timeout_seconds": {Doc: "configured watchdog timeout"},
		"comm":            {Doc: "communicator identifier as reported"},
	},

	ClassGPUDriverMismatch: {
		"gpu_index":               {Doc: "device index on the node"},
		"driver_version":          {Doc: "kernel driver version observed"},
		"expected_driver_version": {Doc: "version expected, from fleet majority or site profile"},
		"cuda_version":            {Doc: "CUDA runtime or userspace library version observed"},
	},
	ClassGPUXid: {
		"gpu_index": {Doc: "device index on the node"},
		"xid":       {Doc: "NVIDIA Xid error number"},
		"pci_addr":  {Doc: "PCI bus address of the device"},
	},
	ClassGPUECC: {
		"gpu_index": {Doc: "device index on the node"},
		"ecc_type":  {Doc: "correctable or uncorrectable"},
		"ecc_count": {Doc: "error count as reported"},
		"pci_addr":  {Doc: "PCI bus address of the device"},
	},
	ClassGPUFallenOffBus: {
		"gpu_index": {Doc: "device index on the node, when still known"},
		"pci_addr":  {Doc: "PCI bus address of the device"},
	},

	ClassStorageMountMissing: {
		"mount":   {Doc: "mount point expected but absent", PII: true},
		"fs_type": {Doc: "filesystem type"},
		"server":  {Doc: "backing server or MGS as configured", PII: true},
	},
	ClassStorageIOError: {
		"mount":   {Doc: "mount point on which the error occurred", PII: true},
		"fs_type": {Doc: "filesystem type"},
		"errno":   {Doc: "symbolic errno, e.g. EIO, ENOSPC"},
	},
	ClassStorageStaleHandle: {
		"mount":   {Doc: "mount point returning stale handles", PII: true},
		"fs_type": {Doc: "filesystem type"},
	},

	ClassConfigClockSkew: {
		"skew_sec":  {Doc: "signed offset of this host's clock from the reference, in seconds"},
		"reference": {Doc: "what the clock was compared against, e.g. chrony, ntp, slurmctld"},
		"daemon":    {Doc: "daemon that reported the skew, e.g. chronyd"},
	},

	ClassConfigDrift: {
		"key":           {Doc: "configuration key that diverged, e.g. nvidia.driver_version"},
		"observed":      {Doc: "value on this node", PII: true},
		"expected":      {Doc: "value held by the sibling majority", PII: true},
		"peer_count":    {Doc: "number of fleet siblings compared against"},
		"peer_majority": {Doc: "number of siblings holding the expected value"},
	},
}

// AttrAllowed reports whether key may appear in the detail of an event of the
// given class.
func AttrAllowed(c Class, key string) bool {
	if _, ok := universalAttrs[key]; ok {
		return true
	}
	_, ok := classAttrs[c][key]
	return ok
}

// AttrIsPII reports whether the value under key carries identifying information
// for the given class, and so must be pseudonymized at the redaction boundary.
//
// Unregistered keys report true. An unknown key is one nobody has assessed, and
// the safe default for unassessed data is to treat it as identifying.
func AttrIsPII(c Class, key string) bool {
	if spec, ok := classAttrs[c][key]; ok {
		return spec.PII
	}
	if spec, ok := universalAttrs[key]; ok {
		return spec.PII
	}
	return true
}

// RegisteredAttrs returns every key valid for the class, universal keys
// included, sorted. Used by the fixture scaffolder and by documentation output.
func RegisteredAttrs(c Class) []string {
	seen := make(map[string]struct{}, len(universalAttrs)+len(classAttrs[c]))
	for k := range universalAttrs {
		seen[k] = struct{}{}
	}
	for k := range classAttrs[c] {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
