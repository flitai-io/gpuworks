package store

import "gorm.io/gorm"

// addDeploymentPriorityScheduling adds the deployment's priority scheduling
// switch. Existing deployments stay off: turning it on changes the engine's
// command, which only a restart the operator chose should do.
func addDeploymentPriorityScheduling(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Deployment{}) || db.Migrator().HasColumn(&Deployment{}, "PriorityScheduling") {
		return nil
	}
	return db.Migrator().AddColumn(&Deployment{}, "PriorityScheduling")
}
