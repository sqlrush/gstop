package model

import "sync"

// DBInfo is the shared container the database monitor fills with the connected
// instance's version, login user, and cluster role. The persistence thread reads
// it to name its log files. It is safe for concurrent use. Port of
// common/data_logger.DBInfo.
type DBInfo struct {
	mu      sync.RWMutex
	version string
	user    string
	role    string
}

// NewDBInfo returns a DBInfo seeded with the same defaults as the Python class.
func NewDBInfo() *DBInfo {
	return &DBInfo{version: "unknown", user: "rdsAdmin", role: "unknown"}
}

// SetVersion records the database version string.
func (d *DBInfo) SetVersion(v string) { d.set(&d.version, v) }

// SetUser records the login user name.
func (d *DBInfo) SetUser(v string) { d.set(&d.user, v) }

// SetRole records the cluster role ("primary" or "standby").
func (d *DBInfo) SetRole(v string) { d.set(&d.role, v) }

func (d *DBInfo) set(field *string, v string) {
	d.mu.Lock()
	*field = v
	d.mu.Unlock()
}

// Version returns the recorded version, or "unknown" until the monitor fills it.
func (d *DBInfo) Version() string { return d.get(&d.version) }

// User returns the recorded login user.
func (d *DBInfo) User() string { return d.get(&d.user) }

// Role returns the recorded cluster role.
func (d *DBInfo) Role() string { return d.get(&d.role) }

func (d *DBInfo) get(field *string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return *field
}
