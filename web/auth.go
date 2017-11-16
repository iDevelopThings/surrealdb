// Copyright © 2016 Abcum Ltd
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package web

import (
	"fmt"
	"net"

	"bytes"
	"strings"

	"encoding/base64"

	"github.com/abcum/fibre"
	"github.com/abcum/surreal/cnf"
	"github.com/abcum/surreal/db"
	"github.com/abcum/surreal/kvs"
	"github.com/abcum/surreal/mem"
	"github.com/abcum/surreal/sql"
	"github.com/dgrijalva/jwt-go"
	"github.com/gorilla/websocket"
)

const (
	varKeyIp     = "ip"
	varKeyNs     = "NS"
	varKeyDb     = "DB"
	varKeySc     = "SC"
	varKeyTk     = "TK"
	varKeyUs     = "US"
	varKeyTb     = "TB"
	varKeyId     = "ID"
	varKeyAuth   = "auth"
	varKeyUser   = "user"
	varKeyPass   = "pass"
	varKeyOrigin = "origin"
)

func cidr(ip net.IP, networks []*net.IPNet) bool {
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func auth() fibre.MiddlewareFunc {
	return func(h fibre.HandlerFunc) fibre.HandlerFunc {
		return func(c *fibre.Context) (err error) {

			auth := &cnf.Auth{}
			c.Set(varKeyAuth, auth)

			// Start off with an authentication level
			// which prevents running any sql queries,
			// and denies access to all data.

			auth.Kind = cnf.AuthNO

			// Set the default possible values for the
			// possible and selected namespace / database
			// so that they can be overridden.

			auth.Possible.NS = ""
			auth.Selected.NS = ""
			auth.Possible.DB = ""
			auth.Selected.DB = ""

			// Retrieve the current domain host and
			// if we are using a subdomain then set
			// the NS and DB to the subdomain bits.

			bits := strings.Split(c.Request().URL().Host, ".")
			subs := strings.Split(bits[0], "-")

			if len(subs) == 2 {
				auth.Possible.NS = subs[0]
				auth.Selected.NS = subs[0]
				auth.Possible.DB = subs[1]
				auth.Selected.DB = subs[1]
			}

			// If there is a namespace specified in
			// the request headers, then mark it as
			// the selected namespace.

			if ns := c.Request().Header().Get(varKeyNs); len(ns) != 0 {
				auth.Possible.NS = ns
				auth.Selected.NS = ns
			}

			// If there is a database specified in
			// the request headers, then mark it as
			// the selected database.

			if db := c.Request().Header().Get(varKeyDb); len(db) != 0 {
				auth.Possible.DB = db
				auth.Selected.DB = db
			}

			// Retrieve the HTTP Authorization header
			// from the request, so that we can detect
			// whether it is Basic auth or Bearer auth.

			head := c.Request().Header().Get("Authorization")

			// If there is no HTTP Authorization header,
			// check if there are websocket subprotocols
			// which might contain authn information.

			if len(head) == 0 {
				for _, prot := range websocket.Subprotocols(c.Request().Request) {
					if len(prot) > 7 && prot[0:7] == "bearer-" {
						return checkBearer(c, prot[7:], func() error {
							return h(c)
						})
					}
				}
			}

			// Check whether the Authorization header
			// is a Basic Auth header, and if it is then
			// process this as root authentication.

			if len(head) > 6 && head[:5] == "Basic" {
				return checkBasics(c, head[6:], func() error {
					return h(c)
				})
			}

			// Check whether the Authorization header
			// is a Bearer Auth header, and if it is then
			// process this as default authentication.

			if len(head) > 7 && head[:6] == "Bearer" {
				return checkBearer(c, head[7:], func() error {
					return h(c)
				})
			}

			return h(c)

		}
	}
}

func checkBasics(c *fibre.Context, info string, callback func() error) (err error) {

	var base []byte
	var cred [][]byte

	auth := c.Get(varKeyAuth).(*cnf.Auth)
	user := []byte(cnf.Settings.Auth.User)
	pass := []byte(cnf.Settings.Auth.Pass)

	// Parse the base64 encoded basic auth data

	if base, err = base64.StdEncoding.DecodeString(info); err != nil {
		return fibre.NewHTTPError(401).WithMessage("Problem with basic auth data")
	}

	// Split the basic auth USER and PASS details

	if cred = bytes.SplitN(base, []byte(":"), 2); len(cred) != 2 {
		return fibre.NewHTTPError(401).WithMessage("Problem with basic auth data")
	}

	// Check to see if IP, USER, and PASS match server settings

	if bytes.Equal(cred[0], user) && bytes.Equal(cred[1], pass) {

		if cidr(c.IP(), cnf.Settings.Auth.Nets) {
			auth.Kind = cnf.AuthKV
			auth.Possible.NS = "*"
			auth.Possible.DB = "*"
			return callback()
		}

		return fibre.NewHTTPError(403).WithMessage("IP invalid for root authentication")

	}

	// If no KV authentication, then try to authenticate as NS user

	if auth.Selected.NS != "" {

		n := auth.Selected.NS
		u := string(cred[0])
		p := string(cred[1])

		if _, err = signinNS(n, u, p); err == nil {
			auth.Kind = cnf.AuthNS
			auth.Possible.NS = n
			auth.Possible.DB = "*"
			return callback()
		}

		// If no NS authentication, then try to authenticate as DB user

		if auth.Selected.DB != "" {

			n := auth.Selected.NS
			d := auth.Selected.DB
			u := string(cred[0])
			p := string(cred[1])

			if _, err = signinDB(n, d, u, p); err == nil {
				auth.Kind = cnf.AuthDB
				auth.Possible.NS = n
				auth.Possible.DB = d
				return callback()
			}

		}

	}

	return fibre.NewHTTPError(401).WithMessage("Invalid authentication details")

}

func checkBearer(c *fibre.Context, info string, callback func() error) (err error) {

	auth := c.Get(varKeyAuth).(*cnf.Auth)

	var txn kvs.TX
	var res []*db.Response
	var vars jwt.MapClaims
	var nsk, dbk, sck, tkk, usk, tbk, idk bool
	var nsv, dbv, scv, tkv, usv, tbv, idv string

	// Start a new read transaction.

	if txn, err = db.Begin(false); err != nil {
		return fibre.NewHTTPError(500)
	}

	// Ensure the transaction closes.

	defer txn.Cancel()

	// Setup the kvs layer cache.

	cache := mem.NewWithTX(txn)

	// Parse the specified JWT Token.

	token, err := jwt.Parse(info, func(token *jwt.Token) (interface{}, error) {

		vars = token.Claims.(jwt.MapClaims)

		if err := vars.Valid(); err != nil {
			return nil, err
		}

		nsv, nsk = vars[varKeyNs].(string) // Namespace
		dbv, dbk = vars[varKeyDb].(string) // Database
		scv, sck = vars[varKeySc].(string) // Scope
		tkv, tkk = vars[varKeyTk].(string) // Token
		usv, usk = vars[varKeyUs].(string) // Login
		tbv, tbk = vars[varKeyTb].(string) // Table
		idv, idk = vars[varKeyId].(string) // Thing

		if tkv == "default" {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("Unexpected signing method")
			}
		}

		if nsk && dbk && sck && tkk {

			scp, err := cache.GetSC(nsv, dbv, scv)
			if err != nil {
				return nil, fmt.Errorf("Credentials failed")
			}

			// Store the authenticated scope.

			auth.Scope = scp.Name.ID

			// Store the authenticated thing.

			auth.Data = sql.NewThing(tbv, idv)

			// Check that the scope specifies connect.

			if exp, ok := scp.Connect.(*sql.SubExpression); ok {

				// Process the scope connect statement.

				c := fibre.NewContext(c.Request(), c.Response(), c.Fibre())

				c.Set(varKeyAuth, &cnf.Auth{Kind: cnf.AuthDB})

				qvars := map[string]interface{}{"id": auth.Data}

				query := &sql.Query{Statements: []sql.Statement{exp.Expr}}

				// If the query fails then fail authentication.

				if res, err = db.Process(c, query, qvars); err != nil {
					return nil, fmt.Errorf("Credentials failed")
				}

				// If the response is not 1 record then fail authentication.

				if len(res) != 1 || len(res[0].Result) != 1 {
					return nil, fmt.Errorf("Credentials failed")
				}

				auth.Data = res[0].Result[0]

			}

			if tkv != "default" {
				key, err := cache.GetST(nsv, dbv, scv, tkv)
				if err != nil {
					return nil, fmt.Errorf("Credentials failed")
				}
				if token.Header["alg"] != key.Type {
					return nil, fmt.Errorf("Unexpected signing method")
				}
				auth.Kind = cnf.AuthSC
				return key.Code, nil
			} else {
				auth.Kind = cnf.AuthSC
				return scp.Code, nil
			}

		} else if nsk && dbk && tkk {

			if tkv != "default" {
				key, err := cache.GetDT(nsv, dbv, tkv)
				if err != nil {
					return nil, fmt.Errorf("Credentials failed")
				}
				if token.Header["alg"] != key.Type {
					return nil, fmt.Errorf("Unexpected signing method")
				}
				auth.Kind = cnf.AuthDB
				return key.Code, nil
			} else if usk {
				usr, err := cache.GetDU(nsv, dbv, usv)
				if err != nil {
					return nil, fmt.Errorf("Credentials failed")
				}
				auth.Kind = cnf.AuthDB
				return usr.Code, nil
			}

		} else if nsk && tkk {

			if tkv != "default" {
				key, err := cache.GetNT(nsv, tkv)
				if err != nil {
					return nil, fmt.Errorf("Credentials failed")
				}
				if token.Header["alg"] != key.Type {
					return nil, fmt.Errorf("Unexpected signing method")
				}
				auth.Kind = cnf.AuthNS
				return key.Code, nil
			} else if usk {
				usr, err := cache.GetNU(nsv, usv)
				if err != nil {
					return nil, fmt.Errorf("Credentials failed")
				}
				auth.Kind = cnf.AuthNS
				return usr.Code, nil
			}

		}

		return nil, fmt.Errorf("No available token")

	})

	if err == nil && token.Valid {

		if auth.Kind == cnf.AuthNS {
			auth.Possible.NS = nsv
			auth.Selected.NS = nsv
			auth.Possible.DB = "*"
		}

		if auth.Kind == cnf.AuthDB {
			auth.Possible.NS = nsv
			auth.Selected.NS = nsv
			auth.Possible.DB = dbv
			auth.Selected.DB = dbv
		}

		if auth.Kind == cnf.AuthSC {
			auth.Possible.NS = nsv
			auth.Selected.NS = nsv
			auth.Possible.DB = dbv
			auth.Selected.DB = dbv
		}

		return callback()

	}

	return fibre.NewHTTPError(401).WithMessage("Invalid authentication details")

}
