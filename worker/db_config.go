/*
 * Copyright 2018 The ThunderDB Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package worker

import (
	"time"

	kt "gitlab.com/thunderdb/ThunderDB/kayak/transport"
	"gitlab.com/thunderdb/ThunderDB/proto"
)

// Config defines the database config.
type Config struct {
	DatabaseID      proto.DatabaseID
	DataDir         string
	MuxService      *kt.ETLSTransportService
	MaxWriteTimeGap time.Duration
}