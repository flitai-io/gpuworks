package modelmeta

import (
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// Outcome is what a model's source said to a fetch that read nothing usable.
// It is decided here, from the status of the requests FetchFrom made, so no
// caller has to read an error's text to tell a missing repository from a
// site that cannot be reached.
type Outcome string

const (
	// OutcomeRepoNotFound: the site answered 404 for the repository itself.
	OutcomeRepoNotFound Outcome = "not_found"
	// OutcomeRevisionNotFound: the site has the repository and answered 404
	// for the revision's files.
	OutcomeRevisionNotFound Outcome = "revision_not_found"
	// OutcomeDenied: the site answered 401 or 403. Hugging Face answers 401
	// for a repository that does not exist too, when asked without a token.
	OutcomeDenied Outcome = "denied"
	// OutcomeUnreachable: no address gave an answer about the repository: the
	// connection failed (DNS, dial, TLS, timeout), or the answer was another
	// error status.
	OutcomeUnreachable Outcome = "unreachable"
)

// Answer is the Outcome with the address that gave it and its HTTP status
// (0 when the request got no response).
type Answer struct {
	Outcome  Outcome
	Endpoint string
	Status   int
	// Disputed: another address of the same source gave no answer about the
	// repository, so this one's "not found" may be a mirror's gap.
	Disputed bool
}

// Definite reports whether the source said, with no address left unheard,
// that it does not have the repository or the revision.
func (a Answer) Definite() bool {
	return !a.Disputed && (a.Outcome == OutcomeRepoNotFound || a.Outcome == OutcomeRevisionNotFound)
}

// answerOf classifies a failed request to endpoint.
func answerOf(endpoint string, err error) Answer {
	answer := Answer{Outcome: OutcomeUnreachable, Endpoint: endpoint, Status: statusOf(err)}
	switch answer.Status {
	case http.StatusNotFound:
		answer.Outcome = OutcomeRepoNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		answer.Outcome = OutcomeDenied
	}
	return answer
}

// statusOf is the HTTP status err carries, 0 when the request got none.
// ModelScope's own Code in a 200 response is not one.
func statusOf(err error) int {
	var own errHTTP
	if errors.As(err, &own) {
		return int(own)
	}
	var hub *modelrepo.StatusError
	if errors.As(err, &hub) && hub.Code >= 100 && hub.Code < 600 {
		return hub.Code
	}
	return 0
}

// unanswered is what a fetch that no address served reports: the last
// address that said something definite about the repository (the source's
// own site comes last among the candidates), else the first failure.
func unanswered(answers []Answer) Answer {
	for i := len(answers) - 1; i >= 0; i-- {
		if answers[i].Outcome != OutcomeUnreachable {
			answer := answers[i]
			for _, other := range answers {
				answer.Disputed = answer.Disputed || other.Outcome == OutcomeUnreachable
			}
			return answer
		}
	}
	if len(answers) > 0 {
		return answers[0]
	}
	return Answer{}
}
