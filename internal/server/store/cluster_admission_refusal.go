package store

import "errors"

// ClusterAdmissionRefusal is ErrClusterAdmissionRejected that says whose
// admission the database refused. One grant covers several members' keys,
// and a refusal of one of them is a verdict on that member only.
type ClusterAdmissionRefusal struct {
	// Key is the fingerprint of the refused member key.
	Key string
}

func (r *ClusterAdmissionRefusal) Error() string { return ErrClusterAdmissionRejected.Error() }

// Is makes the refusal ErrClusterAdmissionRejected for every caller that
// does not ask whose it is.
func (r *ClusterAdmissionRefusal) Is(target error) bool { return target == ErrClusterAdmissionRejected }

// refusedKey names key on an admission refusal that does not yet say whose
// it is; any other error is returned as it is.
func refusedKey(err error, key string) error {
	if err == nil || !errors.Is(err, ErrClusterAdmissionRejected) {
		return err
	}
	var named *ClusterAdmissionRefusal
	if errors.As(err, &named) {
		return err
	}
	return &ClusterAdmissionRefusal{Key: key}
}
