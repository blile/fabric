/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ledgermgmt

import (
	"errors"
	"sync"

	"fmt"

	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/core/ledger/kvledger"
	logging "github.com/op/go-logging"
)

var logger = logging.MustGetLogger("ledgermgmt")

// ErrLedgerAlreadyOpened is thrown by a CreateLedger call if a ledger with the given id is already opened
var ErrLedgerAlreadyOpened = errors.New("Ledger already opened")

// ErrLedgerMgmtNotInitialized is thrown when ledger mgmt is used before initializing this
var ErrLedgerMgmtNotInitialized = errors.New("ledger mgmt should be initialized before using")

var openedLedgers map[string]ledger.PeerLedger
var ledgerProvider ledger.PeerLedgerProvider
var lock sync.Mutex
var initialized bool
var once sync.Once

// Initialize initializes ledgermgmt
func Initialize() {
	once.Do(func() {
		initialize()
	})
}

func initialize() {
	logger.Info("Initializing ledger mgmt")
	lock.Lock()
	defer lock.Unlock()
	initialized = true
	openedLedgers = make(map[string]ledger.PeerLedger)
	provider, err := kvledger.NewProvider()
	if err != nil {
		panic(fmt.Errorf("Error in instantiating ledger provider: %s", err))
	}
	ledgerProvider = provider
	logger.Info("ledger mgmt initialized")
}

// CreateLedger creates a new ledger with the given id
func CreateLedger(id string) (ledger.PeerLedger, error) {
	logger.Infof("Creating leadger with id = %s", id)
	lock.Lock()
	defer lock.Unlock()
	if !initialized {
		return nil, ErrLedgerMgmtNotInitialized
	}
	l, err := ledgerProvider.Create(id)
	if err != nil {
		return nil, err
	}
	l = wrapLedger(id, l)
	openedLedgers[id] = l
	logger.Infof("Created leadger with id = %s", id)
	return l, nil
}

// OpenLedger returns a ledger for the given id
func OpenLedger(id string) (ledger.PeerLedger, error) {
	logger.Infof("Opening leadger with id = %s", id)
	lock.Lock()
	defer lock.Unlock()
	if !initialized {
		return nil, ErrLedgerMgmtNotInitialized
	}
	l, ok := openedLedgers[id]
	if ok {
		return nil, ErrLedgerAlreadyOpened
	}
	l, err := ledgerProvider.Open(id)
	if err != nil {
		return nil, err
	}
	l = wrapLedger(id, l)
	openedLedgers[id] = l
	logger.Infof("Opened leadger with id = %s", id)
	return l, nil
}

// GetLedgerIDs returns the ids of the ledgers created
func GetLedgerIDs() ([]string, error) {
	lock.Lock()
	defer lock.Unlock()
	if !initialized {
		return nil, ErrLedgerMgmtNotInitialized
	}
	return ledgerProvider.List()
}

// Close closes all the opened ledgers and any resources held for ledger management
func Close() {
	logger.Infof("Closing ledger mgmt")
	lock.Lock()
	defer lock.Unlock()
	if !initialized {
		return
	}
	for _, l := range openedLedgers {
		l.(*ClosableLedger).closeWithoutLock()
	}
	ledgerProvider.Close()
	openedLedgers = nil
	logger.Infof("ledger mgmt closed")
}

func wrapLedger(id string, l ledger.PeerLedger) ledger.PeerLedger {
	return &ClosableLedger{id, l}
}

// ClosableLedger extends from actual validated ledger and overwrites the Close method
type ClosableLedger struct {
	id string
	ledger.PeerLedger
}

// Close closes the actual ledger and removes the entries from opened ledgers map
func (l *ClosableLedger) Close() {
	lock.Lock()
	defer lock.Unlock()
	l.closeWithoutLock()
}

func (l *ClosableLedger) closeWithoutLock() {
	l.PeerLedger.Close()
	delete(openedLedgers, l.id)
}
