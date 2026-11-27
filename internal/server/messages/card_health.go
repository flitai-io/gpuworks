package messages

// Card check notices describe availability, not a card's health verdict.
const (
	CardChecksOffline          = "CARD_CHECKS_OFFLINE"
	CardChecksWaiting          = "CARD_CHECKS_WAITING"
	CardChecksStale            = "CARD_CHECKS_STALE"
	CardChecksPassthrough      = "CARD_CHECKS_PASSTHROUGH"
	CardChecksNoDriver         = "CARD_CHECKS_NO_DRIVER"
	CardChecksHeldElsewhere    = "CARD_CHECKS_HELD_ELSEWHERE"
	CardChecksNoUsableCards    = "CARD_CHECKS_NO_USABLE_CARDS"
	CardChecksNoCards          = "CARD_CHECKS_NO_CARDS"
	ErrorCardChecksUnsupported = "CARD_CHECKS_UNSUPPORTED"
)
