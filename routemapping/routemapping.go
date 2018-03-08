package routemapping

import (
	"sync"
)

// RouteMapping contains an internal mapping
type RouteMapping struct {
	mapping   map[string]string
	mutex     *sync.RWMutex
	exclusive bool // Exclusive will force only the mapping to be valid and as the super-mask
}

// New returns a new instance of RouteMapping.
func New(exclusive bool) *RouteMapping {
	return &RouteMapping{
		mapping:   map[string]string{},
		mutex:     &sync.RWMutex{},
		exclusive: exclusive,
	}
}

// Set sets a new set of mappings.
func (m *RouteMapping) Set(mapping map[string]string) {
	m.mutex.Lock()
	m.mapping = mapping
	m.mutex.Unlock()
}

// Get gets all iternal mappings.
func (m *RouteMapping) Get() map[string]string {
	m.mutex.RLock()
	mapping := m.mapping
	m.mutex.RUnlock()
	return mapping
}

// IsExclusive indicates if exclusive is true.
func (m *RouteMapping) IsExclusive() bool {
	return m.exclusive
}
