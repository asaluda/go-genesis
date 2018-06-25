package model

import (
	"github.com/GenesisKernel/go-genesis/packages/converter"
)

// Contract represents record of {prefix}_contracts table
type Contract struct {
	tableName  string
	ID         int64
	Name       string
	Value      string
	WalletID   int64
	TokenID    int64
	Active     bool
	Conditions string
	AppID      int64
}

// SetTablePrefix is setting table prefix
func (c *Contract) SetTablePrefix(prefix string) {
	c.tableName = prefix + "_contracts"
}

// TableName returns name of table
func (c *Contract) TableName() string {
	return c.tableName
}

func (c *Contract) GetList(offset, limit int64) ([]Contract, error) {
	var list []Contract
	err := DBConn.Table(c.tableName).Offset(offset).Limit(limit).Order("id").Find(&list).Error
	return list, err
}

func (c *Contract) ToMap() (v map[string]string) {
	v = make(map[string]string)
	v["id"] = converter.Int64ToStr(c.ID)
	v["name"] = c.Name
	v["value"] = c.Value
	v["wallet_id"] = converter.Int64ToStr(c.WalletID)
	v["token_id"] = converter.Int64ToStr(c.TokenID)
	v["conditions"] = c.Conditions
	v["app_id"] = converter.Int64ToStr(c.AppID)

	if c.Active {
		v["active"] = "1"
	} else {
		v["active"] = "0"
	}

	return
}