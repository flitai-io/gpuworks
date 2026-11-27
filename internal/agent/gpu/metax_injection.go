package gpu

// RuntimeInjection refuses MetaX workloads because selector semantics have
// not been verified for container runtimes.
func (p *MetaXProber) RuntimeInjection(indexes []int) RuntimeInjection {
	if len(indexes) == 0 {
		return RuntimeInjection{}
	}
	return RuntimeInjection{UnsupportedReason: "MetaX container device selector semantics are not verified"}
}
