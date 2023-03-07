// Copyright (C) 2023  Tricorder Observability
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tricorder/src/api-server/http/dao"
	"github.com/tricorder/src/api-server/http/grafana"
	pb "github.com/tricorder/src/api-server/pb"
	testutils "github.com/tricorder/src/testing/bazel"
	grafanatest "github.com/tricorder/src/testing/grafana"
	pgclienttest "github.com/tricorder/src/testing/pg"
	"github.com/tricorder/src/utils/cond"
	"github.com/tricorder/src/utils/lock"
)

var mgr = ModuleManager{DatasourceUID: "test"}

func SetUpRouter() *gin.Engine {
	router := gin.Default()
	testDbFilePath := testutils.GetTmpFile()
	// We'll not cleanup the temp file, as it's troublesome to turn down the http server, and probably not worth it in a
	// test.

	sqliteClient, _ := dao.InitSqlite(testDbFilePath)
	mgr.Module = dao.ModuleDao{
		Client: sqliteClient,
	}

	mgr.ModuleInstance = dao.ModuleInstanceDao{
		Client: sqliteClient,
	}

	mgr.NodeAgent = dao.NodeAgentDao{
		Client: sqliteClient,
	}

	mgr.waitCond = cond.NewCond()
	mgr.gLock = lock.NewLock()

	mgr.GrafanaClient = NewGrafanaManagement()

	return router
}

// test upload wasm file and create wasm uid
func TestModuleManager(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	r := SetUpRouter()

	cleanerFn, grafanaURL, err := grafanatest.LaunchContainer()
	require.Nil(err)
	defer func() { assert.Nil(cleanerFn()) }()

	grafana.InitGrafanaConfig(grafanaURL, "admin", "admin")

	pgClientCleanerFn, pgClient, err := pgclienttest.LaunchContainer()
	require.Nil(err)
	defer func() { assert.Nil(pgClientCleanerFn()) }()

	mgr.PGClient = pgClient

	r.GET("/api/listModule", mgr.listModuleHttp)
	req, _ := http.NewRequest("GET", "/api/listModule?fields=id,name,desire_state", nil)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	resultStr := w.Body.String()
	fmt.Printf("list module: %s", resultStr)
	assert.Equal(true, strings.Contains(resultStr, "Success"))

	wasmUid := "test_wasm_uid"
	modulID := AddModule(t, wasmUid, r)

	r.GET("/api/deployModule", mgr.deployModuleHttp)
	req, err = http.NewRequest("GET", fmt.Sprintf("/api/deployModule?id=%s", modulID), nil)
	assert.Nil(err)

	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	resultStr = w.Body.String()

	assert.Contains(resultStr, "prepare to deploy")

	var deployResult DeployModuleResp
	err = json.Unmarshal([]byte(resultStr), &deployResult)
	assert.Nil(err)

	// check module's status
	moduleResult, err := mgr.Module.QueryByID(modulID)
	assert.Nil(err)
	assert.Equal(int(pb.ModuleState_DEPLOYED), moduleResult.DesireState)

	// check grafana dashboard create result
	ds := grafana.NewDashboard()
	json, err := ds.GetDetailAsJSON(deployResult.UID)
	assert.Nil(err)
	assert.Contains(json, deployResult.UID)

	// check create postgres schema result
	const moduleDataTableNamePrefix = "tricorder_module_"
	err = mgr.PGClient.CheckTableExist(moduleDataTableNamePrefix + moduleResult.ID)
	assert.Nil(err)

	unDeployModule(t, modulID, r)

	deleteModule(t, modulID, r)
}

func AddModule(t *testing.T, wasmUid string, r *gin.Engine) string {
	moduleName := "test_module"
	moduleBody := fmt.Sprintf(`{
		"name": "%s",
		"wasm":{
			"code": "",
			"fn_name":"copy_input_to_output",
			"output_schema":{
				"name":"test_tabel_name",
				"fields":[
					{
						"name":"data",
						"type": 5
					}
				]
			}
		},
		"ebpf":{
			"code": "",
			"perf_buffer_name":"events",
			"probes":[
				{
					"target":"",
					"entry":"sample_json",
					"return":""
				}
			]
		}
	}`, moduleName)

	assert := assert.New(t)

	jsonData := []byte(moduleBody)
	r.POST("/api/addModule", mgr.createModuleHttp)
	req, _ := http.NewRequest("POST", "/api/addModule", bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	resultStr := w.Body.String()
	fmt.Printf("add module: %s", resultStr)
	// check http result
	assert.Contains(resultStr, "success")

	// check db result
	moduleResult, err := mgr.Module.QueryByName(moduleName)
	assert.Nil(err)
	// check whether the name in the database is moduleName
	assert.Equal(moduleName, moduleResult.Name)
	assert.Equal(int(pb.ModuleState_CREATED_), moduleResult.DesireState)
	return moduleResult.ID
}

// Tests that createModuleHttp failed if the input data fields are empty.
func TestCreateModuleEmptyDataFields(t *testing.T) {
	assert := assert.New(t)

	moduleName := "test_module"
	moduleBody := fmt.Sprintf(`{
		"name": "%s",
		"wasm":{
			"code": "",
			"fn_name":"copy_input_to_output",
			"output_schema":{
				"name":"test_tabel_name",
				"fields":[]
			}
		},
		"ebpf":{
			"code": "",
			"perf_buffer_name":"events",
			"probes":[
				{
					"target":"",
					"entry":"sample_json",
					"return":""
				}
			]
		}
	}`, moduleName)

	jsonData := []byte(moduleBody)
	r := SetUpRouter()
	r.POST("/api/createModule", mgr.createModuleHttp)
	req, _ := http.NewRequest("POST", "/api/createModule", bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(`{"code":500,"message":"input data fields cannot be empty"}`, w.Body.String())
}

func deleteModule(t *testing.T, modulID string, r *gin.Engine) {
	r.GET("/api/deleteModule", mgr.deleteModuleHttp)
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/deleteModule?id=%s", modulID), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	resultStr := w.Body.String()
	fmt.Printf("delete module: %s", resultStr)
	assert.Equal(t, true, strings.Contains(resultStr, "Success"))

	resultModule, _ := mgr.Module.QueryByID(modulID)
	if resultModule != nil {
		t.Errorf("delete module by id error:%v", resultModule)
	}
}

func unDeployModule(t *testing.T, modulID string, r *gin.Engine) {
	r.GET("/api/undeployModule", mgr.undeployModuleHttp)
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/undeployModule?id=%s", modulID), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	resultStr := w.Body.String()
	fmt.Printf("un deploy module: %s", resultStr)
	assert.Equal(t, true, strings.Contains(resultStr, "un-deploy success"))

	// check code's status
	resultModule, err := mgr.Module.QueryByID(modulID)
	if err != nil {
		t.Errorf("query module by id error:%v", err)
	}
	assert.Equal(t, int(pb.ModuleState_UNDEPLOYED), resultModule.DesireState)
}
