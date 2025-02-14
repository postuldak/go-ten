package core

import (
	"fmt"
	"math/big"

	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethlog "github.com/ethereum/go-ethereum/log"
	"github.com/ten-protocol/go-ten/go/common/log"
	"github.com/ten-protocol/go-ten/go/common/measure"
)

// VerifySignature - Checks that the L2Tx has a valid signature.
func VerifySignature(chainID int64, tx *types.Transaction) error {
	signer := types.NewLondonSigner(big.NewInt(chainID))
	_, err := types.Sender(signer, tx)
	return err
}

// GetAuthenticatedSender - Get sender and tx nonce from transaction
func GetAuthenticatedSender(chainID int64, tx *types.Transaction) (*gethcommon.Address, error) {
	signer := types.NewLondonSigner(big.NewInt(chainID))
	sender, err := types.Sender(signer, tx)
	if err != nil {
		return nil, err
	}
	return &sender, nil
}

const (
	// log level for requests that take longer than this threshold in millis
	_errorThreshold = 500
	_warnThreshold  = 200
	_infoThreshold  = 100
	_debugThreshold = 50
)

// LogMethodDuration - call only with "defer"
func LogMethodDuration(logger gethlog.Logger, stopWatch *measure.Stopwatch, msg string, args ...any) {
	var f func(msg string, ctx ...interface{})
	durationMillis := stopWatch.Measure().Milliseconds()

	// we adjust the logging level based on the time
	switch {
	case durationMillis > _errorThreshold:
		f = logger.Error
	case durationMillis > _warnThreshold:
		f = logger.Warn
	case durationMillis > _infoThreshold:
		f = logger.Info
	case durationMillis > _debugThreshold:
		f = logger.Debug
	default:
		f = logger.Trace
	}
	newArgs := append([]any{log.DurationKey, stopWatch}, args...)
	f(fmt.Sprintf("LogMethodDuration::%s", msg), newArgs...)
}
