// Copyright 2016 The Xorm Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xorm

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-xorm/builder"
	"github.com/go-xorm/core"
)

const (
	tpStruct = iota
	tpNonStruct
)

// Find retrieve records from table, condiBeans's non-empty fields
// are conditions. beans could be []Struct, []*Struct, map[int64]Struct
// map[int64]*Struct
func (session *Session) Find(rowsSlicePtr interface{}, condiBean ...interface{}) error {
	defer session.resetStatement()
	if session.IsAutoClose {
		defer session.Close()
	}

	sliceValue := reflect.Indirect(reflect.ValueOf(rowsSlicePtr))
	if sliceValue.Kind() != reflect.Slice && sliceValue.Kind() != reflect.Map {
		return errors.New("needs a pointer to a slice or a map")
	}

	sliceElementType := sliceValue.Type().Elem()

	var tp = tpStruct
	if session.Statement.RefTable == nil {
		if sliceElementType.Kind() == reflect.Ptr {
			if sliceElementType.Elem().Kind() == reflect.Struct {
				pv := reflect.New(sliceElementType.Elem())
				session.Statement.setRefValue(pv.Elem())
			} else {
				//return errors.New("slice type")
				tp = tpNonStruct
			}
		} else if sliceElementType.Kind() == reflect.Struct {
			pv := reflect.New(sliceElementType)
			session.Statement.setRefValue(pv.Elem())
		} else {
			//return errors.New("slice type")
			tp = tpNonStruct
		}
	}

	var table = session.Statement.RefTable

	var addedTableName = (len(session.Statement.JoinStr) > 0)
	var autoCond builder.Cond
	if tp == tpStruct {
		if !session.Statement.noAutoCondition && len(condiBean) > 0 {
			var err error
			autoCond, err = session.Statement.buildConds(table, condiBean[0], true, true, false, true, addedTableName)
			if err != nil {
				panic(err)
			}
		} else {
			// !oinume! Add "<col> IS NULL" to WHERE whatever condiBean is given.
			// See https://github.com/go-xorm/xorm/issues/179
			if col := table.DeletedColumn(); col != nil && !session.Statement.unscoped { // tag "deleted" is enabled
				var colName = session.Engine.Quote(col.Name)
				if addedTableName {
					var nm = session.Statement.TableName()
					if len(session.Statement.TableAlias) > 0 {
						nm = session.Statement.TableAlias
					}
					colName = session.Engine.Quote(nm) + "." + colName
				}
				if session.Engine.dialect.DBType() == core.MSSQL {
					autoCond = builder.IsNull{colName}
				} else {
					autoCond = builder.IsNull{colName}.Or(builder.Eq{colName: "0001-01-01 00:00:00"})
				}
			}
		}
	}

	var sqlStr string
	var args []interface{}
	if session.Statement.RawSQL == "" {
		if len(session.Statement.TableName()) <= 0 {
			return ErrTableNotFound
		}

		var columnStr = session.Statement.ColumnStr
		if len(session.Statement.selectStr) > 0 {
			columnStr = session.Statement.selectStr
		} else {
			if session.Statement.JoinStr == "" {
				if columnStr == "" {
					if session.Statement.GroupByStr != "" {
						columnStr = session.Statement.Engine.Quote(strings.Replace(session.Statement.GroupByStr, ",", session.Engine.Quote(","), -1))
					} else {
						columnStr = session.Statement.genColumnStr()
					}
				}
			} else {
				if columnStr == "" {
					if session.Statement.GroupByStr != "" {
						columnStr = session.Statement.Engine.Quote(strings.Replace(session.Statement.GroupByStr, ",", session.Engine.Quote(","), -1))
					} else {
						columnStr = "*"
					}
				}
			}
			if columnStr == "" {
				columnStr = "*"
			}
		}

		condSQL, condArgs, _ := builder.ToSQL(session.Statement.cond.And(autoCond))

		args = append(session.Statement.joinArgs, condArgs...)
		sqlStr = session.Statement.genSelectSQL(columnStr, condSQL)
		// for mssql and use limit
		qs := strings.Count(sqlStr, "?")
		if len(args)*2 == qs {
			args = append(args, args...)
		}
	} else {
		sqlStr = session.Statement.RawSQL
		args = session.Statement.RawParams
	}

	var err error
	if session.canCache() {
		if cacher := session.Engine.getCacher2(table); cacher != nil &&
			!session.Statement.IsDistinct &&
			!session.Statement.unscoped {
			err = session.cacheFind(sliceElementType, sqlStr, rowsSlicePtr, args...)
			if err != ErrCacheFailed {
				return err
			}
			err = nil // !nashtsai! reset err to nil for ErrCacheFailed
			session.Engine.logger.Warn("Cache Find Failed")
		}
	}

	if sliceValue.Kind() != reflect.Map {
		return session.noCacheFind(sliceValue, sqlStr, args...)
	}

	resultsSlice, err := session.query(sqlStr, args...)
	if err != nil {
		return err
	}

	keyType := sliceValue.Type().Key()

	for _, results := range resultsSlice {
		var newValue reflect.Value
		if sliceElementType.Kind() == reflect.Ptr {
			newValue = reflect.New(sliceElementType.Elem())
		} else {
			newValue = reflect.New(sliceElementType)
		}
		err := session.scanMapIntoStruct(newValue.Interface(), results)
		if err != nil {
			return err
		}
		var key interface{}
		// if there is only one pk, we can put the id as map key.
		if len(table.PrimaryKeys) == 1 {
			key, err = str2PK(string(results[table.PrimaryKeys[0]]), keyType)
			if err != nil {
				return err
			}
		} else {
			if keyType.Kind() != reflect.Slice {
				panic("don't support multiple primary key's map has non-slice key type")
			} else {
				var keys core.PK = make([]interface{}, 0, len(table.PrimaryKeys))
				for _, pk := range table.PrimaryKeys {
					skey, err := str2PK(string(results[pk]), keyType)
					if err != nil {
						return err
					}
					keys = append(keys, skey)
				}
				key = keys
			}
		}

		if sliceElementType.Kind() == reflect.Ptr {
			sliceValue.SetMapIndex(reflect.ValueOf(key), reflect.ValueOf(newValue.Interface()))
		} else {
			sliceValue.SetMapIndex(reflect.ValueOf(key), reflect.Indirect(reflect.ValueOf(newValue.Interface())))
		}
	}

	return nil
}

func (session *Session) noCacheFind(sliceValue reflect.Value, sqlStr string, args ...interface{}) error {
	var rawRows *core.Rows
	var err error

	session.queryPreprocess(&sqlStr, args...)
	if session.IsAutoCommit {
		_, rawRows, err = session.innerQuery(sqlStr, args...)
	} else {
		rawRows, err = session.Tx.Query(sqlStr, args...)
	}
	if err != nil {
		return err
	}
	defer rawRows.Close()

	fields, err := rawRows.Columns()
	if err != nil {
		return err
	}

	var newElemFunc func() reflect.Value
	sliceElementType := sliceValue.Type().Elem()
	if sliceElementType.Kind() == reflect.Ptr {
		newElemFunc = func() reflect.Value {
			return reflect.New(sliceElementType.Elem())
		}
	} else {
		newElemFunc = func() reflect.Value {
			return reflect.New(sliceElementType)
		}
	}

	var sliceValueSetFunc func(*reflect.Value)

	if sliceValue.Kind() == reflect.Slice {
		if sliceElementType.Kind() == reflect.Ptr {
			sliceValueSetFunc = func(newValue *reflect.Value) {
				sliceValue.Set(reflect.Append(sliceValue, reflect.ValueOf(newValue.Interface())))
			}
		} else {
			sliceValueSetFunc = func(newValue *reflect.Value) {
				sliceValue.Set(reflect.Append(sliceValue, reflect.Indirect(reflect.ValueOf(newValue.Interface()))))
			}
		}
	}

	var newValue = newElemFunc()
	dataStruct := rValue(newValue.Interface())
	if dataStruct.Kind() == reflect.Struct {
		return session.rows2Beans(rawRows, fields, len(fields), session.Engine.autoMapType(dataStruct), newElemFunc, sliceValueSetFunc)
	}

	for rawRows.Next() {
		var newValue = newElemFunc()
		bean := newValue.Interface()

		if err := rawRows.Scan(bean); err != nil {
			return err
		}

		sliceValueSetFunc(&newValue)
	}
	return nil
}

func (session *Session) cacheFind(t reflect.Type, sqlStr string, rowsSlicePtr interface{}, args ...interface{}) (err error) {
	if !session.canCache() ||
		indexNoCase(sqlStr, "having") != -1 ||
		indexNoCase(sqlStr, "group by") != -1 {
		return ErrCacheFailed
	}

	for _, filter := range session.Engine.dialect.Filters() {
		sqlStr = filter.Do(sqlStr, session.Engine.dialect, session.Statement.RefTable)
	}

	newsql := session.Statement.convertIDSQL(sqlStr)
	if newsql == "" {
		return ErrCacheFailed
	}

	tableName := session.Statement.TableName()

	table := session.Statement.RefTable
	cacher := session.Engine.getCacher2(table)
	ids, err := core.GetCacheSql(cacher, tableName, newsql, args)
	if err != nil {
		rows, err := session.DB().Query(newsql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		var i int
		ids = make([]core.PK, 0)
		for rows.Next() {
			i++
			if i > 500 {
				session.Engine.logger.Debug("[cacheFind] ids length > 500, no cache")
				return ErrCacheFailed
			}
			var res = make([]string, len(table.PrimaryKeys))
			err = rows.ScanSlice(&res)
			if err != nil {
				return err
			}

			var pk core.PK = make([]interface{}, len(table.PrimaryKeys))
			for i, col := range table.PKColumns() {
				if col.SQLType.IsNumeric() {
					n, err := strconv.ParseInt(res[i], 10, 64)
					if err != nil {
						return err
					}
					pk[i] = n
				} else if col.SQLType.IsText() {
					pk[i] = res[i]
				} else {
					return errors.New("not supported")
				}
			}

			ids = append(ids, pk)
		}

		session.Engine.logger.Debug("[cacheFind] cache sql:", ids, tableName, newsql, args)
		err = core.PutCacheSql(cacher, ids, tableName, newsql, args)
		if err != nil {
			return err
		}
	} else {
		session.Engine.logger.Debug("[cacheFind] cache hit sql:", newsql, args)
	}

	sliceValue := reflect.Indirect(reflect.ValueOf(rowsSlicePtr))

	ididxes := make(map[string]int)
	var ides []core.PK
	var temps = make([]interface{}, len(ids))

	for idx, id := range ids {
		sid, err := id.ToString()
		if err != nil {
			return err
		}
		bean := cacher.GetBean(tableName, sid)
		if bean == nil {
			ides = append(ides, id)
			ididxes[sid] = idx
		} else {
			session.Engine.logger.Debug("[cacheFind] cache hit bean:", tableName, id, bean)

			pk := session.Engine.IdOf(bean)
			xid, err := pk.ToString()
			if err != nil {
				return err
			}

			if sid != xid {
				session.Engine.logger.Error("[cacheFind] error cache", xid, sid, bean)
				return ErrCacheFailed
			}
			temps[idx] = bean
		}
	}

	if len(ides) > 0 {
		newSession := session.Engine.NewSession()
		defer newSession.Close()

		slices := reflect.New(reflect.SliceOf(t))
		beans := slices.Interface()

		if len(table.PrimaryKeys) == 1 {
			ff := make([]interface{}, 0, len(ides))
			for _, ie := range ides {
				ff = append(ff, ie[0])
			}

			newSession.In("`"+table.PrimaryKeys[0]+"`", ff...)
		} else {
			for _, ie := range ides {
				cond := builder.NewCond()
				for i, name := range table.PrimaryKeys {
					cond = cond.And(builder.Eq{"`" + name + "`": ie[i]})
				}
				newSession.Or(cond)
			}
		}

		err = newSession.NoCache().Find(beans)
		if err != nil {
			return err
		}

		vs := reflect.Indirect(reflect.ValueOf(beans))
		for i := 0; i < vs.Len(); i++ {
			rv := vs.Index(i)
			if rv.Kind() != reflect.Ptr {
				rv = rv.Addr()
			}
			id := session.Engine.IdOfV(rv)
			sid, err := id.ToString()
			if err != nil {
				return err
			}

			bean := rv.Interface()
			temps[ididxes[sid]] = bean
			session.Engine.logger.Debug("[cacheFind] cache bean:", tableName, id, bean, temps)
			cacher.PutBean(tableName, sid, bean)
		}
	}

	for j := 0; j < len(temps); j++ {
		bean := temps[j]
		if bean == nil {
			session.Engine.logger.Warn("[cacheFind] cache no hit:", tableName, ids[j], temps)
			// return errors.New("cache error") // !nashtsai! no need to return error, but continue instead
			continue
		}
		if sliceValue.Kind() == reflect.Slice {
			if t.Kind() == reflect.Ptr {
				sliceValue.Set(reflect.Append(sliceValue, reflect.ValueOf(bean)))
			} else {
				sliceValue.Set(reflect.Append(sliceValue, reflect.Indirect(reflect.ValueOf(bean))))
			}
		} else if sliceValue.Kind() == reflect.Map {
			var key = ids[j]
			keyType := sliceValue.Type().Key()
			var ikey interface{}
			if len(key) == 1 {
				ikey, err = str2PK(fmt.Sprintf("%v", key[0]), keyType)
				if err != nil {
					return err
				}
			} else {
				if keyType.Kind() != reflect.Slice {
					return errors.New("table have multiple primary keys, key is not core.PK or slice")
				}
				ikey = key
			}

			if t.Kind() == reflect.Ptr {
				sliceValue.SetMapIndex(reflect.ValueOf(ikey), reflect.ValueOf(bean))
			} else {
				sliceValue.SetMapIndex(reflect.ValueOf(ikey), reflect.Indirect(reflect.ValueOf(bean)))
			}
		}
	}

	return nil
}
