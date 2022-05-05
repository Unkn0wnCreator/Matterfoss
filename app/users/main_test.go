// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package users

import (
	"flag"
	"testing"

	"github.com/cjdelisle/matterfoss-server/v6/testlib"
)

var mainHelper *testlib.MainHelper
var replicaFlag bool

func TestMain(m *testing.M) {
	if f := flag.Lookup("mysql-replica"); f == nil {
		flag.BoolVar(&replicaFlag, "mysql-replica", false, "")
		flag.Parse()
	}

	var options = testlib.HelperOptions{
		EnableStore:     true,
		EnableResources: true,
		WithReadReplica: replicaFlag,
	}

	mainHelper = testlib.NewMainHelperWithOptions(&options)
	defer mainHelper.Close()

	mainHelper.Main(m)
}
