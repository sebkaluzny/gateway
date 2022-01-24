package test

import (
	"encoding/json"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	log "github.com/sirupsen/logrus"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"sync"
)

// MarshallJSONToMap is a test utility for serializing a struct to JSON while easily being able to assert on its contents
func MarshallJSONToMap(v interface{}) (map[string]interface{}, error) {
	var result map[string]interface{}

	b, err := json.Marshal(v)
	if err != nil {
		return result, err
	}

	err = json.Unmarshal(b, &result)
	if err != nil {
		return result, err
	}

	return result, nil
}

// Contains is a one-line utility for check key existence in maps for tests
func Contains(m map[string]interface{}, k string) bool {
	_, ok := m[k]
	return ok
}

// NewEthAddress is a one-line utility for deserializing strings into Ethereum addresses
func NewEthAddress(s string) common.Address {
	var address common.Address
	addressBytes, _ := hexutil.Decode(s)
	copy(address[:], addressBytes)
	return address
}

var currentTestPort = 9700
var testPortLock = &sync.Mutex{}

// NextTestPort allocates ports 1 by 1 to allow tests to run concurrently
func NextTestPort() int {
	testPortLock.Lock()
	defer testPortLock.Unlock()

	current := currentTestPort
	currentTestPort++
	return current
}

// GenerateBytes return a random generated byte slice of the specified length
func GenerateBytes(count int) []byte {
	b := make([]byte, count)
	_, _ = rand.Read(b)
	return b
}

var matchFirstCap = regexp.MustCompile("(.)([A-Z][a-z]+)")
var matchAllCap = regexp.MustCompile("([a-z0-9])([A-Z])")

// ToSnakeCase takes a camel case string and returns it in snake case
func ToSnakeCase(str string) string {
	snake := matchFirstCap.ReplaceAllString(str, "${1}_${2}")
	snake = matchAllCap.ReplaceAllString(snake, "${1}_${2}")
	return strings.ToLower(snake)
}

// ConfigureLogger sets the log level for tests. Mainly useful while debugging tests.
func ConfigureLogger(level log.Level) {
	log.SetLevel(level)
	log.SetOutput(os.Stdout)
}
