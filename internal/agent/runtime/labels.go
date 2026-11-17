package runtime

import "sort"

// LabelArgs renders labels as `--label key=value` run flags in key order, so
// the same spec always produces the same command line. Empty values are
// skipped: an unknown fact is left off rather than written as "".
func LabelArgs(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for k, v := range labels {
		if k != "" && v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	args := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		args = append(args, "--label", k+"="+labels[k])
	}
	return args
}
