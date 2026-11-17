package store

import "gorm.io/gorm"

// addNodeRuntimeFallback adds the runtime a node's auto-detection passed
// over and why (Node.RuntimeFallbackFrom/Reason). Both stay empty until each
// node's next report says otherwise.
func addNodeRuntimeFallback(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Node{}) {
		return nil
	}
	for _, field := range []string{"RuntimeFallbackFrom", "RuntimeFallbackReason"} {
		if db.Migrator().HasColumn(&Node{}, field) {
			continue
		}
		if err := db.Migrator().AddColumn(&Node{}, field); err != nil {
			return err
		}
	}
	return nil
}

// runtimeFallbackSocketFields are the columns of a socket the agent's
// account may not open (Node.RuntimeFallbackSocket*).
var runtimeFallbackSocketFields = []string{
	"RuntimeFallbackSocket", "RuntimeFallbackSocketGroup", "RuntimeFallbackSocketGID",
	"RuntimeFallbackSocketGroupHeld",
	"RuntimeFallbackSocketAccount", "RuntimeFallbackSocketSameNameGID",
	"RuntimeFallbackSocketRestartPending", "RuntimeFallbackSocketSupervisor",
	"RuntimeFallbackSocketStateDir",
}

// addNodeRuntimeFallbackSocket adds what the agent found about a daemon
// socket its account may not open. All stay empty until each node's next
// report says otherwise.
func addNodeRuntimeFallbackSocket(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Node{}) {
		return nil
	}
	for _, field := range runtimeFallbackSocketFields {
		if db.Migrator().HasColumn(&Node{}, field) {
			continue
		}
		if err := db.Migrator().AddColumn(&Node{}, field); err != nil {
			return err
		}
	}
	return nil
}
