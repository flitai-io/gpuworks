package store

import "gorm.io/gorm"

// upperCaseCheckCodes rewrites the stored codes of card checks in the one
// spelling ADR 0067 gives them, UPPER_SNAKE: a card's finding code
// (gpus.health_code) and the code of each card health failure record
// (card_health_episodes.reason). Older servers stored them in lower_snake as
// the agent sent them. Every write since is already UPPER_SNAKE (the hub folds
// an older agent's report on the way in), so after this the stored data has
// one spelling and no reader normalizes. The check statuses stored beside
// them (card_health_episodes.level: warn, fail) were already the check
// status words.
func upperCaseCheckCodes(db *gorm.DB) error {
	for _, rewrite := range []struct{ table, stmt string }{
		{"gpus", `UPDATE gpus SET health_code = UPPER(health_code) WHERE health_code <> UPPER(health_code)`},
		{"card_health_episodes", `UPDATE card_health_episodes SET reason = UPPER(reason) WHERE reason <> UPPER(reason)`},
	} {
		// A schema assembled for one area's migrations may lack the table;
		// it then has no rows to rewrite.
		if !db.Migrator().HasTable(rewrite.table) {
			continue
		}
		if err := db.Exec(rewrite.stmt).Error; err != nil {
			return err
		}
	}
	return nil
}
