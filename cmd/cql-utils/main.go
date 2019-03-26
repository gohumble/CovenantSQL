/*
 * Copyright 2018 The CovenantSQL Authors.
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

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh/terminal"

	"github.com/CovenantSQL/CovenantSQL/utils"
	"github.com/CovenantSQL/CovenantSQL/utils/log"
)

var (
	version        = "unknown"
	tool           string
	publicKeyHex   string
	privateKeyFile string
	configFile     string
	skipMasterKey  bool
	showVersion    bool
)

const name = "cql-utils"

func init() {
	log.SetLevel(log.InfoLevel)

	flag.StringVar(&tool, "tool", "", "Tool type, miner, nonce, confgen")
	flag.StringVar(&publicKeyHex, "public", "", "Public key hex string to mine node id/nonce")
	flag.StringVar(&privateKeyFile, "private", "~/.cql/private.key", "Private key file to generate/show")
	flag.StringVar(&configFile, "config", "~/.cql/config.yaml", "Config file to use")
	flag.BoolVar(&skipMasterKey, "skip-master-key", false, "Use empty master key")
	flag.BoolVar(&showVersion, "version", false, "Show version information and exit")
}

func main() {
	flag.Parse()
	if showVersion {
		fmt.Printf("%v %v %v %v %v\n",
			name, version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		os.Exit(0)
	}
	log.Infof("cql-utils build: %#v\n", version)

	configFile = utils.HomeDirExpand(configFile)
	privateKeyFile = utils.HomeDirExpand(privateKeyFile)

	switch tool {
	case "miner":
		if publicKeyHex == "" && privateKeyFile == "" {
			// error
			log.Error("publicKey or privateKey is required in miner mode")
			os.Exit(1)
		}
		runMiner()
	// Disable keygen independent call
	//case "keygen":
	//	if privateKeyFile == "" {
	//		// error
	//		log.Error("privateKey path is required for keygen")
	//		os.Exit(1)
	//	}
	//	runKeygen()
	case "nonce":
		runNonce()
	case "confgen":
		runConfgen()
	default:
		flag.Usage()
		os.Exit(1)
	}
}

func readMasterKey() (string, error) {
	if skipMasterKey {
		return "", nil
	}
	fmt.Println("Enter master key(press Enter for default: \"\"): ")
	bytePwd, err := terminal.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	return string(bytePwd), err
}

func askDeletePath(path string) {
	if _, err := os.Stat(path); err == nil {
		reader := bufio.NewReader(os.Stdin)
		fmt.Printf("\"%s\" already exists. \nDo you want to delete it? (y or n, press Enter for default n):\n",
			path)
		t, err := reader.ReadString('\n')
		t = strings.Trim(t, "\n")
		if err != nil {
			log.WithError(err).Error("unexpected error")
			os.Exit(1)
		}
		if strings.Compare(t, "y") == 0 || strings.Compare(t, "yes") == 0 {
			err = os.RemoveAll(path)
			if err != nil {
				log.WithError(err).Error("unexpected error")
				os.Exit(1)
			}
		} else {
			os.Exit(0)
		}
	}
}
