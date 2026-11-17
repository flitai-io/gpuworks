package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/priority"
)

// PriorityClass is a gateway request priority class: a name clients and keys
// refer to, and a value (larger is more important) the gateway turns into the
// engine's request priority (docs/explanation/request-priority.md). The
// built-in classes are seeded by migration; their values are fixed. Exactly
// one class is the default: the class of a request whose key names none, or
// that has no key.
type PriorityClass struct {
	ID          uint   `gorm:"primaryKey"`
	Name        string `gorm:"uniqueIndex;size:32;not null"`
	DisplayName string `gorm:"size:64;not null;default:''"`
	Description string `gorm:"size:256;not null;default:''"`
	Value       int    `gorm:"not null"`
	Builtin     bool   `gorm:"not null;default:false"`
	IsDefault   bool   `gorm:"not null;default:false"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Priority class refusals the API maps to its own errors.
var (
	ErrPriorityClassExists  = errors.New("a priority class with this name already exists")
	ErrPriorityClassBuiltin = errors.New("a built-in priority class keeps its name and value and cannot be deleted")
	ErrPriorityClassInUse   = errors.New("the priority class is the default or is bound to an API key")
	ErrPriorityClassInvalid = errors.New("invalid priority class")
	// ErrPriorityClassGone: a batch or evaluation run names a class that no
	// longer exists when it is written.
	ErrPriorityClassGone = errors.New("the priority class does not exist")
)

// priorityClassEvent is the bus topic class changes publish on (for
// listeners such as the console's live views; the gateway follows
// PriorityClassGeneration instead).
const priorityClassEvent = "priority_class"

// createPriorityClasses creates the table with the built-in classes (normal
// the default) and the API key binding columns. Keys from before stay
// unbound, so their requests run at the default, as they ran at the neutral
// level before.
func createPriorityClasses(db *gorm.DB) error {
	if err := db.AutoMigrate(&PriorityClass{}); err != nil {
		return err
	}
	for _, b := range priority.Builtins {
		var count int64
		if err := db.Model(&PriorityClass{}).Where("name = ?", b.Name).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		row := PriorityClass{Name: b.Name, Value: b.Value, Builtin: true, IsDefault: b.Name == priority.Normal}
		if err := db.Create(&row).Error; err != nil {
			return err
		}
	}
	if !db.Migrator().HasTable(&APIKey{}) {
		return nil
	}
	for _, col := range []string{"PriorityClassID", "PriorityCeilingClassID", "PriorityPolicy"} {
		if !db.Migrator().HasColumn(&APIKey{}, col) {
			if err := db.Migrator().AddColumn(&APIKey{}, col); err != nil {
				return err
			}
		}
	}
	// AddColumn does not make the columns' indexes a fresh table has (a
	// class delete looks the bound keys up by them).
	for _, col := range []string{"PriorityClassID", "PriorityCeilingClassID"} {
		if !db.Migrator().HasIndex(&APIKey{}, col) {
			if err := db.Migrator().CreateIndex(&APIKey{}, col); err != nil {
				return err
			}
		}
	}
	return nil
}

// PriorityClassGeneration changes whenever this process changes a class or
// the default, so the gateway's snapshot reloads at once; another server's
// changes reach it by the snapshot's age.
func (s *Store) PriorityClassGeneration() uint64 { return s.priorityGen.Load() }

func (s *Store) priorityChanged() { s.priorityGen.Add(1) }

// PriorityClasses lists the classes, most important first.
func (s *Store) PriorityClasses() ([]PriorityClass, error) {
	var rows []PriorityClass
	err := s.reader().Order("value desc, name").Find(&rows).Error
	return rows, err
}

// PriorityClassTable is the classes as the gateway's rules see them.
func (s *Store) PriorityClassTable() (priority.Table, error) {
	rows, err := s.PriorityClasses()
	if err != nil {
		return priority.Table{}, err
	}
	classes := make([]priority.Class, 0, len(rows))
	var defaultID uint
	for _, r := range rows {
		classes = append(classes, priority.Class{ID: r.ID, Name: r.Name, Value: r.Value})
		if r.IsDefault {
			defaultID = r.ID
		}
	}
	return priority.NewTable(classes, defaultID), nil
}

func validPriorityClass(c *PriorityClass) error {
	c.Name = strings.TrimSpace(c.Name)
	c.DisplayName, c.Description = strings.TrimSpace(c.DisplayName), strings.TrimSpace(c.Description)
	if !priority.ValidName(c.Name) {
		return fmt.Errorf("%w: name %q", ErrPriorityClassInvalid, c.Name)
	}
	if !priority.ValidValue(c.Value) {
		return fmt.Errorf("%w: value %d", ErrPriorityClassInvalid, c.Value)
	}
	return nil
}

// CreatePriorityClass adds a class an administrator defined; it is never
// built in and never the default until made so.
func (s *Store) CreatePriorityClass(c *PriorityClass) error {
	c.ID, c.Builtin, c.IsDefault = 0, false, false
	if err := validPriorityClass(c); err != nil {
		return err
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&PriorityClass{}).Where("name = ?", c.Name).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrPriorityClassExists
		}
		return tx.Create(c).Error
	})
	if err == nil {
		s.priorityChanged()
		s.Bus.PublishChange(priorityClassEvent, c.Name, "created")
	}
	return err
}

// PriorityClassUpdate is what an edit of a class may change; nil leaves it.
type PriorityClassUpdate struct {
	DisplayName *string
	Description *string
	Value       *int
}

// UpdatePriorityClass edits a class. A built-in class keeps its value.
func (s *Store) UpdatePriorityClass(name string, update PriorityClassUpdate) (PriorityClass, error) {
	var out PriorityClass
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("name = ?", name).First(&out).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if update.Value != nil && *update.Value != out.Value {
			if out.Builtin {
				return ErrPriorityClassBuiltin
			}
			out.Value = *update.Value
		}
		if update.DisplayName != nil {
			out.DisplayName = strings.TrimSpace(*update.DisplayName)
		}
		if update.Description != nil {
			out.Description = strings.TrimSpace(*update.Description)
		}
		if err := validPriorityClass(&out); err != nil {
			return err
		}
		return tx.Model(&PriorityClass{}).Where("id = ?", out.ID).Updates(map[string]any{
			"display_name": out.DisplayName, "description": out.Description, "value": out.Value,
		}).Error
	})
	if err == nil {
		s.priorityChanged()
		s.Bus.PublishChange(priorityClassEvent, name, "updated")
	}
	return out, err
}

// DeletePriorityClass removes a class no live key is bound to; a built-in
// class or the default is refused. Revoked keys that named it lose the
// binding, which no request can use any more.
// PriorityClassesBound is the classes a live API key names, as its class or
// its ceiling: DeletePriorityClass refuses them.
func (s *Store) PriorityClassesBound() (map[uint]bool, error) {
	var rows []struct {
		Class, Ceiling *uint
	}
	if err := s.reader().Model(&APIKey{}).Select("priority_class_id AS class, priority_ceiling_class_id AS ceiling").
		Where("revoked_at IS NULL AND (priority_class_id IS NOT NULL OR priority_ceiling_class_id IS NOT NULL)").Scan(&rows).Error; err != nil {
		return nil, err
	}
	bound := map[uint]bool{}
	for _, r := range rows {
		for _, id := range []*uint{r.Class, r.Ceiling} {
			if id != nil {
				bound[*id] = true
			}
		}
	}
	var named []uint
	if err := s.reader().Model(&PriorityClass{}).Where("name IN (?) OR name IN (?)", unfinishedBatchClasses(s.reader()), activeBenchmarkClasses(s.reader())).
		Pluck("id", &named).Error; err != nil {
		return nil, err
	}
	for _, id := range named {
		bound[id] = true
	}
	return bound, nil
}

// unfinishedBatchClasses and activeBenchmarkClasses are the class names a
// batch still sending requests, or an evaluation run still to send them,
// runs at: deleting one of those classes would change their requests' class
// part way.
func unfinishedBatchClasses(db *gorm.DB) *gorm.DB {
	return db.Model(&Batch{}).Select("priority_class").
		Where("priority_class <> '' AND status IN ?", []string{BatchValidating, BatchInProgress, BatchFinalizing, BatchCancelling})
}

// requirePriorityClass fails with ErrPriorityClassGone unless a class of
// the name exists; "" names none. It runs in the transaction that writes the
// row naming it, so a delete cannot slip between the check and the write.
func requirePriorityClass(tx *gorm.DB, name string) error {
	if name == "" {
		return nil
	}
	var n int64
	if err := tx.Model(&PriorityClass{}).Where("name = ?", name).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return ErrPriorityClassGone
	}
	return nil
}

func activeBenchmarkClasses(db *gorm.DB) *gorm.DB {
	return db.Model(&BenchmarkRun{}).Select("priority_class").
		Where("priority_class <> '' AND state IN ?", []string{"", "pending", "running"})
}

func (s *Store) DeletePriorityClass(name string) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var c PriorityClass
		if err := tx.Where("name = ?", name).First(&c).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if c.Builtin {
			return ErrPriorityClassBuiltin
		}
		if c.IsDefault {
			return ErrPriorityClassInUse
		}
		var bound int64
		if err := tx.Model(&APIKey{}).Where("revoked_at IS NULL AND (priority_class_id = ? OR priority_ceiling_class_id = ?)", c.ID, c.ID).Count(&bound).Error; err != nil {
			return err
		}
		if bound > 0 {
			return ErrPriorityClassInUse
		}
		for _, users := range []*gorm.DB{unfinishedBatchClasses(tx), activeBenchmarkClasses(tx)} {
			if err := users.Where("priority_class = ?", c.Name).Count(&bound).Error; err != nil {
				return err
			}
			if bound > 0 {
				return ErrPriorityClassInUse
			}
		}
		for _, col := range []string{"priority_class_id", "priority_ceiling_class_id"} {
			if err := tx.Model(&APIKey{}).Where(col+" = ?", c.ID).Update(col, nil).Error; err != nil {
				return err
			}
		}
		return tx.Delete(&PriorityClass{}, c.ID).Error
	})
	if err == nil {
		s.apiKeys.changed()
		s.priorityChanged()
		s.Bus.PublishChange(priorityClassEvent, name, "deleted")
	}
	return err
}

// SetDefaultPriorityClass makes name the default class.
func (s *Store) SetDefaultPriorityClass(name string) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var c PriorityClass
		if err := tx.Where("name = ?", name).First(&c).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := tx.Model(&PriorityClass{}).Where("is_default = ? AND id <> ?", true, c.ID).Update("is_default", false).Error; err != nil {
			return err
		}
		return tx.Model(&PriorityClass{}).Where("id = ?", c.ID).Update("is_default", true).Error
	})
	if err == nil {
		s.priorityChanged()
		s.Bus.PublishChange(priorityClassEvent, name, "default")
	}
	return err
}

// APIKeyPriority is a key's priority binding by class name: Class "" binds
// none (the default), Ceiling "" caps at the key's own class. Only the
// within-ceiling policy has a ceiling; another policy's is dropped.
type APIKeyPriority struct {
	Class   string
	Ceiling string
	Policy  priority.Policy
}

// SetAPIKeyPriority binds a live key's requests to a class. An unknown key is
// ErrNotFound; an unknown class ErrPriorityClassInvalid. The gateway applies
// it from the key's next request.
func (s *Store) SetAPIKeyPriority(prefix string, binding APIKeyPriority) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var k APIKey
		if err := tx.Where("prefix = ? AND revoked_at IS NULL", prefix).First(&k).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		classID, err := priorityClassIDByName(tx, binding.Class)
		if err != nil {
			return err
		}
		if !PriorityPolicyTakesCeiling(binding.Policy) {
			binding.Ceiling = ""
		}
		ceilingID, err := priorityClassIDByName(tx, binding.Ceiling)
		if err != nil {
			return err
		}
		if ceilingID != nil {
			// A ceiling below the key's own class (the default's when it
			// binds none) would be ignored; refuse it rather than store it.
			own, ceiling, err := bindingValues(tx, classID, *ceilingID)
			if err != nil {
				return err
			}
			if ceiling < own {
				return fmt.Errorf("%w: ceiling %q is below the key's class", ErrPriorityClassInvalid, binding.Ceiling)
			}
		}
		policy := binding.Policy
		if policy == priority.DefaultPolicy {
			policy = ""
		}
		return tx.Model(&APIKey{}).Where("id = ?", k.ID).Updates(map[string]any{
			"priority_class_id": classID, "priority_ceiling_class_id": ceilingID, "priority_policy": string(policy),
		}).Error
	})
	s.apiKeys.forget(prefix)
	if err == nil {
		s.Bus.PublishChange("apikey", prefix, "updated")
	}
	return err
}

// bindingValues are the values of a binding's own class (the default class
// when classID is nil) and of its ceiling.
func bindingValues(tx *gorm.DB, classID *uint, ceilingID uint) (own, ceiling int, err error) {
	var rows []PriorityClass
	if err := tx.Select("id", "value", "is_default").Find(&rows).Error; err != nil {
		return 0, 0, err
	}
	for _, r := range rows {
		if (classID != nil && r.ID == *classID) || (classID == nil && r.IsDefault) {
			own = r.Value
		}
		if r.ID == ceilingID {
			ceiling = r.Value
		}
	}
	return own, ceiling, nil
}

func priorityClassIDByName(tx *gorm.DB, name string) (*uint, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	var c PriorityClass
	if err := tx.Select("id").Where("name = ?", name).First(&c).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("%w: no class %q", ErrPriorityClassInvalid, name)
		}
		return nil, err
	}
	return &c.ID, nil
}

// priorityBinding is the key's binding as the gateway's rules read it.
func (k APIKey) priorityBinding() priority.Binding {
	policy, ok := priority.ParsePolicy(k.PriorityPolicy)
	if !ok {
		policy = priority.DefaultPolicy
	}
	var out priority.Binding
	out.Policy = policy
	if k.PriorityClassID != nil {
		out.ClassID = *k.PriorityClassID
	}
	if k.PriorityCeilingClassID != nil {
		out.CeilingID = *k.PriorityCeilingClassID
	}
	return out
}

// The class rules the management API states and checks, re-exported so the
// API reads them where it reads the classes (package priority holds them).
const (
	PriorityClassMinValue = priority.MinValue
	PriorityClassMaxValue = priority.MaxValue
)

// ValidPriorityClassName reports whether name may name a class.
func ValidPriorityClassName(name string) bool { return priority.ValidName(name) }

// ValidPriorityClassValue reports whether value is within the class bounds.
func ValidPriorityClassValue(value int) bool { return priority.ValidValue(value) }

// PriorityPolicies are the key policies in display order, and
// DefaultPriorityPolicy the policy of a key that states none.
func PriorityPolicies() []string {
	out := make([]string, 0, len(priority.Policies))
	for _, p := range priority.Policies {
		out = append(out, string(p))
	}
	return out
}

// DefaultPriorityPolicy is the policy of a key that states none.
const DefaultPriorityPolicy = string(priority.DefaultPolicy)

// PriorityPolicyTakesCeiling reports whether a binding of policy keeps a
// ceiling (SetAPIKeyPriority drops another policy's).
func PriorityPolicyTakesCeiling(policy priority.Policy) bool {
	return policy == priority.PolicyWithinCeiling || policy == ""
}

// PriorityCeilingPolicies are the policies a ceiling applies to.
func PriorityCeilingPolicies() []string { return []string{string(priority.PolicyWithinCeiling)} }

// ParsePriorityPolicy reads a requested policy; empty is the default.
func ParsePriorityPolicy(text string) (priority.Policy, bool) { return priority.ParsePolicy(text) }

// PriorityBindingPolicy is a key's stored policy as the API states it.
func PriorityBindingPolicy(stored string) string {
	if p, ok := priority.ParsePolicy(stored); ok {
		return string(p)
	}
	return DefaultPriorityPolicy
}
