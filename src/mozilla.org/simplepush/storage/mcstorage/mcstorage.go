/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package mcstorage

// thin memcache wrapper

/** TODO: Support multiple memcache nodes.
 *      * Need to be able to discover and shard to each node.
 */

import (
	"github.com/bradfitz/gomemcache/memcache"
	"mozilla.org/simplepush/sperrors"
	"mozilla.org/util"

	"encoding/json"
    "errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DELETED = iota
	LIVE
	REGISTERED
)

var config util.JsMap

type Storage struct {
	config util.JsMap
	mc     *memcache.Client
	log    *util.HekaLogger
	thrash int64
}

type StorageError struct {
	err   error
	retry int
}

func (e *StorageError) Error() string {
	return "StorageError: " + e.err.Error()
}

func indexOf(list []string, val string) (index int) {
	for index, v := range list {
		if v == val {
			return index
		}
	}
	return -1
}

func (self *Storage) isFatal(err error) bool {
	// if it has anything to do with the connection, restart the server.
	// this is crappy, crappy behavior, but it's what go wants.
    if err == nil {
        return false
    }
    if strings.Contains(err.Error(), "timeout") {
        return false
    }
    if strings.Contains(err.Error(), "too many open files") {
        return false
    }
	switch err {
	case nil:
		return false
	case memcache.ErrCacheMiss, memcache.ErrCASConflict,
		memcache.ErrNotStored, memcache.ErrNoStats,
		memcache.ErrMalformedKey:
		return false
	default:
		self.log.Critical("storage", "CRITICAL HIT! RESTARTING!",
			util.JsMap{"error": err})
		log.Fatal("### RESTARTING ### ", err)
		return true
	}
}

func ResolvePK(pk string) (uaid, appid string, err error) {
	items := strings.SplitN(pk, ".", 2)
	if len(items) < 2 {
		return pk, "", nil
	}
	return items[0], items[1], nil
}

func GenPK(uaid, appid string) (pk string, err error) {
	pk = fmt.Sprintf("%s.%s", uaid, appid)
	return pk, nil
}

func (self *Storage) fetchRec(pk string) (result util.JsMap, err error) {
	if pk == "" {
		err = sperrors.InvalidPrimaryKeyError
		return nil, err
	}

	defer func() {
		if err := recover(); err != nil {
			self.isFatal(err.(error))
			self.log.Error("storage",
				fmt.Sprintf("could not fetch record for %s", pk),
				util.JsMap{"primarykey": pk, "error": err})
		}
	}()

    mc := memcache.New(self.config["memcache.server"].(string))
    //mc.Timeout = time.Second * 10
		item, err := mc.Get(string(pk))
		if err != nil {
			self.isFatal(err)
			self.log.Error("storage",
				"Get Failed",
				util.JsMap{"primarykey": pk,
					"error": err})
			return nil, err
		}

		json.Unmarshal(item.Value, &result)

		if result == nil {
			return nil, err
		}

		self.log.Debug("storage",
			"Fetched",
			util.JsMap{"primarykey": pk,
				"item":  item,
				"value": item.Value})
		return result, err
}

func (self *Storage) fetchAppIDArray(uaid string) (result []string, err error) {
	if uaid == "" {
		return result, nil
	}
    mc := memcache.New(self.config["memcache.server"].(string))
    //mc.Timeout = time.Second * 10
		raw, err := mc.Get(uaid)
		if err != nil {
			self.isFatal(err)
			return nil, err
		}
	    result = strings.Split(string(raw.Value), ",")
    	return result, err
}

func (self *Storage) storeAppIDArray(uaid string, arr sort.StringSlice) (err error) {
	arr.Sort()
    mc := memcache.New(self.config["memcache.server"].(string))
    //mc.Timeout = time.Second * 10
		err = mc.Set(&memcache.Item{Key: uaid,
			Value:      []byte(strings.Join(arr, ",")),
			Expiration: 0})
		if err != nil {
			self.isFatal(err)
		}
		return err
}

func (self *Storage) storeRec(pk string, rec util.JsMap) (err error) {
	if pk == "" {
		return sperrors.InvalidPrimaryKeyError
	}

	if rec == nil {
		err = sperrors.NoDataToStoreError
		return err
	}

	raw, err := json.Marshal(rec)

	if err != nil {
		self.log.Error("storage",
			"storeRec marshalling failure",
			util.JsMap{"error": err,
				"primarykey": pk,
				"record":     rec})
		return err
	}

	var ttls string
	switch rec["s"] {
	case DELETED:
		ttls = config["db.timeout_del"].(string)
	case REGISTERED:
		ttls = config["db.timeout_reg"].(string)
	default:
		ttls = config["db.timeout_live"].(string)
	}
	rec["l"] = time.Now().UTC().Unix()

	ttl, err := strconv.ParseInt(ttls, 0, 0)
	self.log.Debug("storage",
		"Storing record",
		util.JsMap{"primarykey": pk,
			"record": raw})
	item := &memcache.Item{Key: pk,
		Value:      []byte(raw),
		Expiration: int32(ttl)}

    mc := memcache.New(self.config["memcache.server"].(string))
    //mc.Timeout = time.Second * 10
		err = mc.Set(item)
		if err != nil {
			self.isFatal(err)
			self.log.Error("storage",
				fmt.Sprintf("Failure to set item %s {%s}", pk, item),
				nil)
		}
	return err
}

func New(opts util.JsMap, log *util.HekaLogger) *Storage {

	config = opts
	var ok bool

	if _, ok = config["memcache.server"]; !ok {
		config["memcache.server"] = "127.0.0.1:11211"
	}

	if _, ok = config["db.timeout_live"]; !ok {
		config["db.timeout_live"] = "259200"
	}

	if _, ok = config["db.timeout_reg"]; !ok {
		config["db.timeout_reg"] = "10800"
	}

	if _, ok = config["db.timeout_del"]; !ok {
		config["db.timeout_del"] = "86400"
	}
	if _, ok = config["shard.default_host"]; !ok {
		config["shard.default_host"] = "localhost"
	}
	if _, ok = config["shard.current_host"]; !ok {
		config["shard.current_host"] = config["shard.default_host"]
	}
	if _, ok = config["shard.prefix"]; !ok {
		config["shard.prefix"] = "_h-"
	}

	log.Info("storage", "Creating new memcache handler", nil)
	return &Storage{mc: nil,
		config: config,
		log:    log,
		thrash: 0}
}

//TODO: Optimize this to decode the PK for updates
func (self *Storage) UpdateChannel(pk string, vers int64) (err error) {

	var rec util.JsMap

	if len(pk) == 0 {
		return sperrors.InvalidPrimaryKeyError
	}

	rec, err = self.fetchRec(pk)

	if err != nil && err != memcache.ErrCacheMiss {
		return err
	}

	if rec != nil {
		self.log.Debug("storage", fmt.Sprintf("Found record for %s", pk), nil)
		if rec["s"] != DELETED {
			newRecord := make(util.JsMap)
			newRecord["v"] = vers
			newRecord["s"] = LIVE
			newRecord["l"] = time.Now().UTC().Unix()
			err = self.storeRec(pk, newRecord)
			if err != nil {
				return err
			}
			return nil
		}
	}
	// No record found or the record setting was DELETED
	uaid, appid, err := ResolvePK(pk)
	self.log.Debug("storage",
		"Registering channel",
		util.JsMap{"uaid": uaid,
			"channelID": appid,
			"version":   vers})
	err = self.RegisterAppID(uaid, appid, vers)
	if err == sperrors.ChannelExistsError {
		pk, err = GenPK(uaid, appid)
		if err != nil {
			return err
		}
		return self.UpdateChannel(pk, vers)
	}
	return err
}

func (self *Storage) RegisterAppID(uaid, appid string, vers int64) (err error) {

	var rec util.JsMap

	if len(appid) == 0 {
		return sperrors.NoChannelError
	}

	appIDArray, err := self.fetchAppIDArray(uaid)
	// Yep, this should eventually be optimized to a faster scan.
	if appIDArray != nil {
		appIDArray = remove(appIDArray, indexOf(appIDArray, appid))
	}
	err = self.storeAppIDArray(uaid, append(appIDArray, appid))
	if err != nil {
		return err
	}

	rec = make(util.JsMap)
	rec["s"] = REGISTERED
	rec["l"] = time.Now().UTC().Unix()
	if vers != 0 {
		rec["v"] = vers
		rec["s"] = LIVE
	}

	pk, err := GenPK(uaid, appid)
	if err != nil {
		return err
	}

	err = self.storeRec(pk, rec)
	if err != nil {
		return err
	}
	return nil
}

func remove(list []string, pos int) (res []string) {
	if pos < 0 {
		return list
	}
	if pos == len(list) {
		return list[:pos]
	}
	return append(list[:pos], list[pos+1:]...)
}

func (self *Storage) DeleteAppID(uaid, appid string, clearOnly bool) (err error) {

	if len(appid) == 0 {
		return sperrors.NoChannelError
	}

	appIDArray, err := self.fetchAppIDArray(uaid)
	if err != nil {
		return err
	}
	pos := sort.SearchStrings(appIDArray, appid)
	if pos > -1 {
		self.storeAppIDArray(uaid, remove(appIDArray, pos))
		pk, err := GenPK(uaid, appid)
		if err != nil {
			return err
		}
		rec, err := self.fetchRec(pk)
		if err == nil {
			rec["s"] = DELETED
			err = self.storeRec(pk, rec)
		} else {
			self.log.Error("storage",
				"Could not delete Channel",
				util.JsMap{"primarykey": pk, "error": err})
		}
	} else {
		err = sperrors.InvalidChannelError
	}
	return err
}

func (self *Storage) IsKnownUaid(uaid string) bool {
	self.log.Debug("storage", "IsKnownUaid", util.JsMap{"uaid": uaid})
	_, err := self.fetchAppIDArray(uaid)
	if err == nil {
		return true
	}
	return false
}

func (self *Storage) GetUpdates(uaid string, lastAccessed int64) (results util.JsMap, err error) {
	appIDArray, err := self.fetchAppIDArray(uaid)

	var updates []map[string]interface{}
	var expired []string
	var items []string

	for _, appid := range appIDArray {
		pk, _ := GenPK(uaid, appid)
		// TODO: Puke on error
		items = append(items, pk)
	}
	self.log.Debug("storage",
		"Fetching items",
		util.JsMap{"uaid": uaid,
			"items": items})
    mc := memcache.New(self.config["memcache.server"].(string))

		recs, err := mc.GetMulti(items)
		if err != nil {
			self.isFatal(err)
			self.log.Error("storage", "GetUpdate failed",
				util.JsMap{"uaid": uaid,
					"error": err})
			return nil, err
		}

	var update util.JsMap
	if len(recs) == 0 {
		self.log.Debug("storage",
			"GetUpdates No records found", util.JsMap{"uaid": uaid})
		return nil, err
	}
	for _, rec := range recs {
		uaid, appid, err := ResolvePK(rec.Key)
		self.log.Debug("storage",
			"GetUpdates Fetched record ",
			util.JsMap{"uaid": uaid,
				"value": rec.Value})
		err = json.Unmarshal(rec.Value, &update)
		if err != nil {
			return nil, err
		}
		if int64(update["l"].(float64)) < lastAccessed {
			self.log.Debug("storage", "Skipping record...", util.JsMap{"rec": update})
			continue
		}
		// Yay! Go translates numeric interface values as float64s
		// Apparently float64(1) != int(1).
		switch update["s"] {
		case float64(LIVE):
			var fvers float64
			var ok bool
			// log.Printf("Adding record... %s", appid)
			newRec := make(util.JsMap)
			newRec["channelID"] = appid
			fvers, ok = update["v"].(float64)
			if !ok {
				var cerr error
				self.log.Warn("storage",
					"GetUpdates Possibly bad version",
					util.JsMap{"update": update})
				fvers, cerr = strconv.ParseFloat(update["v"].(string), 0)
				if cerr != nil {
					self.log.Error("storage",
						"GetUpdates Using Timestamp",
						util.JsMap{"update": update})
					fvers = float64(time.Now().UTC().Unix())
				}
			}
			newRec["version"] = int64(fvers)
			updates = append(updates, newRec)
		case float64(DELETED):
			self.log.Info("storage",
				"GetUpdates Deleting record",
				util.JsMap{"update": update})
			expired = append(expired, appid)
		case float64(REGISTERED):
			// Item registered, but not yet active. Ignore it.
		default:
			self.log.Warn("storage",
				"Unknown state",
				util.JsMap{"update": update})
		}

	}
	if len(expired) == 0 && len(updates) == 0 {
		return nil, nil
	}
	results = make(util.JsMap)
	results["expired"] = expired
	results["updates"] = updates
	return results, err
}

func (self *Storage) Ack(uaid string, ackPacket map[string]interface{}) (err error) {
	//TODO, go through the results and nuke what's there, then call flush

    mc := memcache.New(self.config["memcache.server"].(string))
    //mc.Timeout = time.Second * 10
	if _, ok := ackPacket["expired"]; ok {
		if ackPacket["expired"] != nil {
			expired := make([]string, strings.Count(ackPacket["expired"].(string), ",")+1)
			json.Unmarshal(ackPacket["expired"].([]byte), &expired)
			for _, appid := range expired {
				pk, _ := GenPK(uaid, appid)
					err = mc.Delete(pk)
					if err != nil {
						self.isFatal(err)
					}
			}
		}
	}
	if _, ok := ackPacket["updates"]; ok {
		if ackPacket["updates"] != nil {
			// unspool the loaded records.
			for _, rec := range ackPacket["updates"].([]interface{}) {
				recmap := rec.(map[string]interface{})
				pk, _ := GenPK(uaid, recmap["channelID"].(string))
					err = mc.Delete(pk)
					if err != nil {
						self.isFatal(err)
					}
			}
		}
	}

	if err != nil && err != memcache.ErrCacheMiss {
		return err
	}
	return nil
}

func (self *Storage) ReloadData(uaid string, updates []string) (err error) {
	//TODO: This is not really required.
	_, _ = uaid, updates
	return nil
}

func (self *Storage) SetUAIDHost(uaid string) (err error) {
	host := self.config["shard.current_host"].(string)
	prefix := self.config["shard.prefix"].(string)

	if uaid == "" {
		return sperrors.MissingDataError
	}

	self.log.Debug("storage",
		"SetUAIDHost",
		util.JsMap{"uaid": uaid, "host": host})
	ttl, _ := strconv.ParseInt(self.config["db.timeout_live"].(string), 0, 0)
    mc := memcache.New(self.config["memcache.server"].(string))
    mc.Timeout = time.Second * 10
		err = mc.Set(&memcache.Item{Key: prefix + uaid,
			Value:      []byte(host),
			Expiration: int32(ttl)})
		self.isFatal(err)
	return err
}

func (self *Storage) GetUAIDHost(uaid string) (host string, err error) {
	defaultHost := self.config["shard.default_host"].(string)
	prefix := self.config["shard.prefix"].(string)

	defer func(defaultHost string) {
		if err := recover(); err != nil {
			self.log.Error("storage",
				"GetUAIDHost no host",
				util.JsMap{"uaid": uaid,
					"error": err})
		}
	}(defaultHost)

	var item *memcache.Item
    mc := memcache.New(self.config["memcache.server"].(string))
    //mc.Timeout = time.Second * 10
		item, err = mc.Get(prefix + uaid)
	if err != nil {
		self.isFatal(err)
		self.log.Error("storage",
			"GetUAIDHost Fetch error",
			util.JsMap{"uaid": uaid,
				"item":  item,
				"error": err})
		return defaultHost, err
	}
	self.log.Debug("storage",
		"GetUAIDHost",
		util.JsMap{"uaid": uaid,
			"host": string(item.Value)})
	// reinforce the link.
	self.SetUAIDHost(string(item.Value))
	return string(item.Value), nil
}

func (self *Storage) PurgeUAID(uaid string) (err error) {
	appIDArray, err := self.fetchAppIDArray(uaid)
    mc := memcache.New(self.config["memcache.server"].(string))
	if err == nil && len(appIDArray) > 0 {
		for _, appid := range appIDArray {
			pk, _ := GenPK(uaid, appid)
				err = mc.Delete(pk)
		}
	}
		err = mc.Delete(uaid)
	self.DelUAIDHost(uaid)
	return nil
}

func (self *Storage) DelUAIDHost(uaid string) (err error) {
	prefix := self.config["shard.prefix"].(string)
    mc := memcache.New(self.config["memcache.server"].(string))
    //mc.Timeout = time.Second * 10
		err = mc.Delete(prefix + uaid)
		self.isFatal(err)
	return err
}

func (self *Storage) Status() (success bool, err error) {
    defer func() {
        if recv := recover(); recv != nil {
            success = false
            err = recv.(error)
            return
        }
    }()

    //Test memcache
    fake_id, _ := util.GenUUID4()
    key := "status_" + fake_id
    mc := memcache.New(self.config["memcache.server"].(string))
    err = mc.Set(&memcache.Item{Key: "status_" + fake_id,
                                Value: []byte("test"),
                                Expiration: 6})
    if err != nil {
        return false, err
    }
    item, err := mc.Get(key)
    if err != nil || string(item.Value) != "test" {
        return false, errors.New("Invalid value returned")
    }
    mc.Delete(key)
    return true, nil
}
/*
func (self *Storage) Handler(chan in) {
    for {
        select {
        case cmd := <-in:
            select cmd['cmd'].string(){
                case "DelUAIDHost":
                    cmd["err"] = DelUAIDHost(cmd["uaid"])
                case "PurgeUAID":
                    cmd["err"] = PurgeUAID(cmd["uaid"])
                case "GetUAIDHost":
                    cmd["host"], cmd["err"] = GetUAIDHost(cmd["uaid")
                case "SetUAIDHost":
                    cmd["err"] = SetUAIDHost(cmd["uaid"], cmd["host"])
                case "Ack":
                    cmd["err"] = Ack(cmd["uaid"], cmd["ackPacket"])
                case "GetUpdates":
                    cmd["updates"] = GetUpdates(cmd["uaid"], cmd["lastAccessed"])
                case "IsKnownUaid":
                    cmd["known"] = IsKnownUaid(cmd["uaid"])
                case "DeleteAppId":
                    cmd["err"] = DeleteAppId(cmd["uaid"], cmd["channelid"], cmd["clearOnly"])
                case "RegisterAppID":
                    cmd["err"] = RegisterAppID(cmd["uaid"], cmd["channelid"], cmd["vers"])
                case "UpdateChannel":
                    cmd["err"] = UpdateChannel(cmd["pk"], cmd["version"])
            }
            in<- cmd
        }
    }
}
*/

// o4fs
// vim: set tabstab=4 softtabstop=4 shiftwidth=4 noexpandtab