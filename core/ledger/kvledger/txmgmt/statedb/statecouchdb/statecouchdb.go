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

package statecouchdb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/statedb"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/version"
	"github.com/hyperledger/fabric/core/ledger/ledgerconfig"
	"github.com/hyperledger/fabric/core/ledger/util/couchdb"
	logging "github.com/op/go-logging"
)

var logger = logging.MustGetLogger("statecouchdb")

var compositeKeySep = []byte{0x00}
var lastKeyIndicator = byte(0x01)
var savePointKey = []byte{0x00}

// VersionedDBProvider implements interface VersionedDBProvider
type VersionedDBProvider struct {
	couchInstance *couchdb.CouchInstance
	databases     map[string]*VersionedDB
	mux           sync.Mutex
	openCounts    uint64
}

// NewVersionedDBProvider instantiates VersionedDBProvider
func NewVersionedDBProvider() (*VersionedDBProvider, error) {
	logger.Debugf("constructing CouchDB VersionedDBProvider")
	couchDBDef := ledgerconfig.GetCouchDBDefinition()
	couchInstance, err := couchdb.CreateCouchInstance(couchDBDef.URL, couchDBDef.Username, couchDBDef.Password)
	if err != nil {
		return nil, err
	}

	return &VersionedDBProvider{couchInstance, make(map[string]*VersionedDB), sync.Mutex{}, 0}, nil
}

// GetDBHandle gets the handle to a named database
func (provider *VersionedDBProvider) GetDBHandle(dbName string) (statedb.VersionedDB, error) {
	provider.mux.Lock()
	defer provider.mux.Unlock()

	//TODO determine if couch db naming restrictions should apply to all ledger names
	//TODO enforce all naming rules
	// Only lowercase characters (a-z), digits (0-9), and any of the characters _, $, (, ), +, -, and / are allowed. Must begin with a letter.
	// For now, we'll just lowercase the name within the couch versioned db.
	dbName = strings.ToLower(dbName)

	vdb := provider.databases[dbName]
	if vdb == nil {
		var err error
		vdb, err = newVersionedDB(provider.couchInstance, dbName)
		if err != nil {
			return nil, err
		}
		provider.databases[dbName] = vdb
	}
	return vdb, nil
}

// Close closes the underlying db instance
func (provider *VersionedDBProvider) Close() {
	// No close needed on Couch
}

// VersionedDB implements VersionedDB interface
type VersionedDB struct {
	db     *couchdb.CouchDatabase
	dbName string
}

// newVersionedDB constructs an instance of VersionedDB
func newVersionedDB(couchInstance *couchdb.CouchInstance, dbName string) (*VersionedDB, error) {
	// CreateCouchDatabase creates a CouchDB database object, as well as the underlying database if it does not exist
	db, err := couchdb.CreateCouchDatabase(*couchInstance, dbName)
	if err != nil {
		return nil, err
	}
	return &VersionedDB{db, dbName}, nil
}

// Open implements method in VersionedDB interface
func (vdb *VersionedDB) Open() error {
	// no need to open db since a shared couch instance is used
	return nil
}

// Close implements method in VersionedDB interface
func (vdb *VersionedDB) Close() {
	// no need to close db since a shared couch instance is used
}

// GetState implements method in VersionedDB interface
func (vdb *VersionedDB) GetState(namespace string, key string) (*statedb.VersionedValue, error) {
	logger.Debugf("GetState(). ns=%s, key=%s", namespace, key)

	compositeKey := constructCompositeKey(namespace, key)

	docBytes, _, err := vdb.db.ReadDoc(string(compositeKey))
	if err != nil {
		return nil, err
	}
	if docBytes == nil {
		return nil, nil
	}

	// trace the first 200 bytes of value only, in case it is huge
	if docBytes != nil && logger.IsEnabledFor(logging.DEBUG) {
		if len(docBytes) < 200 {
			logger.Debugf("getCommittedValueAndVersion() Read docBytes %s", docBytes)
		} else {
			logger.Debugf("getCommittedValueAndVersion() Read docBytes %s...", docBytes[0:200])
		}
	}

	ver := version.NewHeight(1, 1) //TODO - version hardcoded to 1 is a temporary value for the prototype

	return &statedb.VersionedValue{Value: docBytes, Version: ver}, nil
}

// GetStateMultipleKeys implements method in VersionedDB interface
func (vdb *VersionedDB) GetStateMultipleKeys(namespace string, keys []string) ([]*statedb.VersionedValue, error) {

	vals := make([]*statedb.VersionedValue, len(keys))
	for i, key := range keys {
		val, err := vdb.GetState(namespace, key)
		if err != nil {
			return nil, err
		}
		vals[i] = val
	}
	return vals, nil

}

// GetStateRangeScanIterator implements method in VersionedDB interface
// startKey is inclusive
// endKey is exclusive
func (vdb *VersionedDB) GetStateRangeScanIterator(namespace string, startKey string, endKey string) (statedb.ResultsIterator, error) {

	compositeStartKey := constructCompositeKey(namespace, startKey)
	compositeEndKey := constructCompositeKey(namespace, endKey)
	if endKey == "" {
		compositeEndKey[len(compositeEndKey)-1] = lastKeyIndicator
	}
	queryResult, err := vdb.db.ReadDocRange(string(compositeStartKey), string(compositeEndKey), 1000, 0)
	if err != nil {
		logger.Debugf("Error calling ReadDocRange(): %s\n", err.Error())
		return nil, err
	}
	logger.Debugf("Exiting GetStateRangeScanIterator")
	return newKVScanner(namespace, *queryResult), nil

}

// ExecuteQuery implements method in VersionedDB interface
func (vdb *VersionedDB) ExecuteQuery(query string) (statedb.ResultsIterator, error) {

	//TODO - limit is currently set at 1000,  eventually this will need to be changed
	//to reflect a config option and potentially return an exception if the threshold is exceeded
	// skip (paging) is not utilized by fabric
	queryResult, err := vdb.db.QueryDocuments(query, 1000, 0)
	if err != nil {
		logger.Debugf("Error calling QueryDocuments(): %s\n", err.Error())
		return nil, err
	}
	logger.Debugf("Exiting ExecuteQuery")
	return newQueryScanner(*queryResult), nil
}

// ApplyUpdates implements method in VersionedDB interface
func (vdb *VersionedDB) ApplyUpdates(batch *statedb.UpdateBatch, height *version.Height) error {

	for ck, vv := range batch.KVs {
		compositeKey := constructCompositeKey(ck.Namespace, ck.Key)

		// trace the first 200 characters of versioned value only, in case it is huge
		if logger.IsEnabledFor(logging.DEBUG) {
			versionedValueDump := fmt.Sprintf("%#v", vv)
			if len(versionedValueDump) > 200 {
				versionedValueDump = versionedValueDump[0:200] + "..."
			}
			logger.Debugf("Applying key=%#v, versionedValue=%s", ck, versionedValueDump)
		}

		// TODO add delete logic for couch using this approach from stateleveldb - convert nils to deletes
		/*		if vv.Value == nil {
					levelBatch.Delete(compositeKey)
				} else {
					levelBatch.Put(compositeKey, encodeValue(vv.Value, vv.Version))
				}
		*/

		if couchdb.IsJSON(string(vv.Value)) {

			// SaveDoc using couchdb client and use JSON format
			rev, err := vdb.db.SaveDoc(string(compositeKey), "", vv.Value, nil)
			if err != nil {
				logger.Errorf("Error during Commit(): %s\n", err.Error())
				return err
			}
			if rev != "" {
				logger.Debugf("Saved document revision number: %s\n", rev)
			}

		} else { // if the data is not JSON, save as binary attachment in Couch

			//Create an attachment structure and load the bytes
			attachment := &couchdb.Attachment{}
			attachment.AttachmentBytes = vv.Value
			attachment.ContentType = "application/octet-stream"
			attachment.Name = "valueBytes"

			attachments := []couchdb.Attachment{}
			attachments = append(attachments, *attachment)

			// SaveDoc using couchdb client and use attachment to persist the binary data
			rev, err := vdb.db.SaveDoc(string(compositeKey), "", nil, attachments)
			if err != nil {
				logger.Errorf("Error during Commit(): %s\n", err.Error())
				return err
			}
			if rev != "" {
				logger.Debugf("Saved document revision number: %s\n", rev)
			}

		}
	}

	// Record a savepoint at a given height
	err := vdb.recordSavepoint(height)
	if err != nil {
		logger.Errorf("Error during recordSavepoint: %s\n", err.Error())
		return err
	}

	return nil
}

// Savepoint docid (key) for couchdb
const savepointDocID = "statedb_savepoint"

// Savepoint data for couchdb
type couchSavepointData struct {
	BlockNum  uint64 `json:"BlockNum"`
	TxNum     uint64 `json:"TxNum"`
	UpdateSeq string `json:"UpdateSeq"`
}

// recordSavepoint Record a savepoint in statedb.
// Couch parallelizes writes in cluster or sharded setup and ordering is not guaranteed.
// Hence we need to fence the savepoint with sync. So ensure_full_commit is called before AND after writing savepoint document
// TODO: Optimization - merge 2nd ensure_full_commit with savepoint by using X-Couch-Full-Commit header
func (vdb *VersionedDB) recordSavepoint(height *version.Height) error {
	var err error
	var savepointDoc couchSavepointData
	// ensure full commit to flush all changes until now to disk
	dbResponse, err := vdb.db.EnsureFullCommit()
	if err != nil || dbResponse.Ok != true {
		logger.Errorf("Failed to perform full commit\n")
		return errors.New("Failed to perform full commit")
	}

	// construct savepoint document
	// UpdateSeq would be useful if we want to get all db changes since a logical savepoint
	dbInfo, _, err := vdb.db.GetDatabaseInfo()
	if err != nil {
		logger.Errorf("Failed to get DB info %s\n", err.Error())
		return err
	}
	savepointDoc.BlockNum = height.BlockNum
	savepointDoc.TxNum = height.TxNum
	savepointDoc.UpdateSeq = dbInfo.UpdateSeq

	savepointDocJSON, err := json.Marshal(savepointDoc)
	if err != nil {
		logger.Errorf("Failed to create savepoint data %s\n", err.Error())
		return err
	}

	// SaveDoc using couchdb client and use JSON format
	_, err = vdb.db.SaveDoc(savepointDocID, "", savepointDocJSON, nil)
	if err != nil {
		logger.Errorf("Failed to save the savepoint to DB %s\n", err.Error())
		return err
	}

	// ensure full commit to flush savepoint to disk
	dbResponse, err = vdb.db.EnsureFullCommit()
	if err != nil || dbResponse.Ok != true {
		logger.Errorf("Failed to perform full commit\n")
		return errors.New("Failed to perform full commit")
	}
	return nil
}

// GetLatestSavePoint implements method in VersionedDB interface
func (vdb *VersionedDB) GetLatestSavePoint() (*version.Height, error) {

	var err error
	savepointJSON, _, err := vdb.db.ReadDoc(savepointDocID)
	if err != nil {
		logger.Errorf("Failed to read savepoint data %s\n", err.Error())
		return &version.Height{BlockNum: 0, TxNum: 0}, err
	}

	// ReadDoc() not found (404) will result in nil response, in these cases return height 0
	if savepointJSON == nil {
		return &version.Height{BlockNum: 0, TxNum: 0}, nil
	}

	savepointDoc := &couchSavepointData{}
	err = json.Unmarshal(savepointJSON, &savepointDoc)
	if err != nil {
		logger.Errorf("Failed to unmarshal savepoint data %s\n", err.Error())
		return &version.Height{BlockNum: 0, TxNum: 0}, err
	}

	return &version.Height{BlockNum: savepointDoc.BlockNum, TxNum: savepointDoc.TxNum}, nil
}

func constructCompositeKey(ns string, key string) []byte {
	compositeKey := []byte(ns)
	compositeKey = append(compositeKey, compositeKeySep...)
	compositeKey = append(compositeKey, []byte(key)...)
	return compositeKey
}

func splitCompositeKey(compositeKey []byte) (string, string) {
	split := bytes.SplitN(compositeKey, compositeKeySep, 2)
	return string(split[0]), string(split[1])
}

type kvScanner struct {
	cursor    int
	namespace string
	results   []couchdb.QueryResult
}

func newKVScanner(namespace string, queryResults []couchdb.QueryResult) *kvScanner {
	return &kvScanner{-1, namespace, queryResults}
}

func (scanner *kvScanner) Next() (statedb.QueryResult, error) {

	scanner.cursor++

	if scanner.cursor >= len(scanner.results) {
		return nil, nil
	}

	selectedKV := scanner.results[scanner.cursor]

	_, key := splitCompositeKey([]byte(selectedKV.ID))

	//TODO - change hardcoded version (1,1) when version header is available in CouchDB
	return &statedb.VersionedKV{
		CompositeKey:   statedb.CompositeKey{Namespace: scanner.namespace, Key: key},
		VersionedValue: statedb.VersionedValue{Value: selectedKV.Value, Version: version.NewHeight(1, 1)}}, nil
}

func (scanner *kvScanner) Close() {
	scanner = nil
}

type queryScanner struct {
	cursor  int
	results []couchdb.QueryResult
}

func newQueryScanner(queryResults []couchdb.QueryResult) *queryScanner {
	return &queryScanner{-1, queryResults}
}

func (scanner *queryScanner) Next() (statedb.QueryResult, error) {

	scanner.cursor++

	if scanner.cursor >= len(scanner.results) {
		return nil, nil
	}

	selectedResultRecord := scanner.results[scanner.cursor]

	namespace, key := splitCompositeKey([]byte(selectedResultRecord.ID))

	//TODO - change hardcoded version (1,1) when version support is available in CouchDB
	return &statedb.VersionedQueryRecord{
		Namespace: namespace,
		Key:       key,
		Version:   version.NewHeight(1, 1),
		Record:    selectedResultRecord.Value}, nil
}

func (scanner *queryScanner) Close() {
	scanner = nil
}
