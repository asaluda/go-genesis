package model

// Menu is model
type Menu struct {
	tableName  string
	ID         int64  `gorm:"primary_key;not null" json:"id"`
	Name       string `gorm:"not null" json:"name"`
	Title      string `gorm:"not null" json:"title"`
	Value      string `gorm:"not null" json:"value"`
	Conditions string `gorm:"not null" json:"conditions"`
}

// SetTablePrefix is setting table prefix
func (m *Menu) SetTablePrefix(prefix string) {
	m.tableName = prefix + "_menu"
}

// TableName returns name of table
func (m Menu) TableName() string {
	return m.tableName
}

// Get is retrieving model from database
func (m *Menu) Get(name string) (bool, error) {
	return isFound(DBConn.Where("name = ?", name).First(m))
}
