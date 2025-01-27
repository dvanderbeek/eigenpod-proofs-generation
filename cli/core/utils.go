package core

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"os"
	"sort"
	"strconv"

	eigenpodproofs "github.com/Layr-Labs/eigenpod-proofs-generation"
	"github.com/Layr-Labs/eigenpod-proofs-generation/cli/core/onchain"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	gethCommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/params"
	"github.com/fatih/color"
)

const (
	ValidatorStatusInactive  = 0
	ValidatorStatusActive    = 1
	ValidatorStatusWithdrawn = 2
)

func Panic(message string) {
	color.Red(fmt.Sprintf("error: %s\n\n", message))

	os.Exit(1)
}

func WeiToGwei(val *big.Int) *big.Float {
	return new(big.Float).Quo(
		new(big.Float).SetInt(val),
		big.NewFloat(params.GWei),
	)
}

func GweiToEther(val *big.Float) *big.Float {
	return new(big.Float).Quo(val, big.NewFloat(params.GWei))
}

func GweiToWei(val *big.Float) *big.Float {
	return new(big.Float).Mul(val, big.NewFloat(params.GWei))
}

func IweiToEther(val *big.Int) *big.Float {
	return new(big.Float).Quo(new(big.Float).SetInt(val), big.NewFloat(params.Ether))
}

func PanicOnError(message string, err error) {
	if err != nil {
		color.Red(fmt.Sprintf("error: %s\n\n", message))

		info := color.New(color.FgRed, color.Italic)
		info.Printf(fmt.Sprintf("caused by: %s\n", err))

		os.Exit(1)
	}
}

func chunk[T any](arr []T, chunkSize uint64) [][]T {
	// Validate the chunkSize to ensure it's positive
	if chunkSize <= 0 {
		panic("chunkSize must be greater than 0")
	}

	// Create a slice to hold the chunks
	var chunks [][]T

	// Loop through the input slice and create chunks
	arrLen := uint64(len(arr))
	for i := uint64(0); i < arrLen; i += chunkSize {
		end := uint64(i + chunkSize)
		if end > arrLen {
			end = arrLen
		}
		chunks = append(chunks, arr[i:end])
	}

	return chunks
}

type ValidatorWithIndex = struct {
	Validator *phase0.Validator
	Index     uint64
}

func withDryRun(opts *bind.TransactOpts) *bind.TransactOpts {
	// golang doesn't have a spread operator for structs smh
	return &bind.TransactOpts{
		From:   opts.From,
		Nonce:  opts.Nonce,
		Signer: opts.Signer,

		Value:     opts.Value,
		GasPrice:  opts.GasPrice,
		GasFeeCap: opts.GasFeeCap,
		GasTipCap: opts.GasTipCap,
		GasLimit:  0, // Gas limit to set for the transaction execution (0 = estimate)

		Context: opts.Context, // Network context to support cancellation and timeouts (nil = no timeout)
		NoSend:  true,         // Do all transact steps but do not send the transaction
	}
}

type Owner = struct {
	FromAddress        gethCommon.Address
	PublicKey          *ecdsa.PublicKey
	TransactionOptions *bind.TransactOpts
	IsDryRun           bool
}

func StartCheckpoint(ctx context.Context, eigenpodAddress string, ownerPrivateKey string, chainId *big.Int, eth *ethclient.Client, forceCheckpoint bool, noSend bool) (*types.Transaction, error) {
	ownerAccount, err := PrepareAccount(&ownerPrivateKey, chainId, noSend)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	eigenPod, err := onchain.NewEigenPod(gethCommon.HexToAddress(eigenpodAddress), eth)
	if err != nil {
		return nil, fmt.Errorf("failed to reach eigenpod: %w", err)
	}

	revertIfNoBalance := !forceCheckpoint

	txn, err := eigenPod.StartCheckpoint(ownerAccount.TransactionOptions, revertIfNoBalance)
	if err != nil {
		if !forceCheckpoint {
			return nil, fmt.Errorf("failed to start checkpoint (try running again with `--force`): %w", err)
		}

		return nil, fmt.Errorf("failed to start checkpoint: %w", err)
	}

	return txn, nil
}

func GetBeaconClient(beaconUri string, verbose bool) (BeaconClient, error) {
	beaconClient, _, err := NewBeaconClient(beaconUri, verbose)
	return beaconClient, err
}

func GetCurrentCheckpoint(eigenpodAddress string, client *ethclient.Client) (uint64, error) {
	eigenPod, err := onchain.NewEigenPod(common.HexToAddress(eigenpodAddress), client)
	if err != nil {
		return 0, fmt.Errorf("failed to locate eigenpod. is your address correct?: %w", err)
	}

	timestamp, err := eigenPod.CurrentCheckpointTimestamp(nil)
	if err != nil {
		return 0, fmt.Errorf("failed to locate eigenpod. Is your address correct?: %w", err)
	}

	return timestamp, nil
}

// Fetch and return the current checkpoint timestamp for the pod
// If the checkpoint exists (timestamp != 0), also return the beacon state for the checkpoint
// If the checkpoint does not exist (timestamp == 0), return the head beacon state (i.e. the state we would use "if we start a checkpoint now")
func GetCheckpointTimestampAndBeaconState(
	ctx context.Context,
	eigenpodAddress string,
	eth *ethclient.Client,
	beaconClient BeaconClient,
) (uint64, *spec.VersionedBeaconState, error) {
	tracing := GetContextTracingCallbacks(ctx)

	tracing.OnStartSection("GetCurrentCheckpoint", map[string]string{})
	checkpointTimestamp, err := GetCurrentCheckpoint(eigenpodAddress, eth)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to fetch current checkpoint: %w", err)
	}
	tracing.OnEndSection()

	// stateId to look up beacon state. "head" by default (if we do not have a checkpoint)
	beaconStateId := "head"

	// If we have a checkpoint, get the state id for the checkpoint's block root
	if checkpointTimestamp != 0 {
		// Fetch the checkpoint's block root
		tracing.OnStartSection("GetCurrentCheckpointBlockRoot", map[string]string{})
		blockRoot, err := GetCurrentCheckpointBlockRoot(eigenpodAddress, eth)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to fetch last checkpoint: %w", err)
		}
		if blockRoot == nil {
			return 0, nil, fmt.Errorf("failed to fetch last checkpoint - nil blockRoot")
		}
		// Block root should be nonzero because we have an active checkpoint
		rootBytes := *blockRoot
		if AllZero(rootBytes[:]) {
			return 0, nil, fmt.Errorf("failed to fetch last checkpoint - empty blockRoot")
		}
		tracing.OnEndSection()

		headerBlock := "0x" + hex.EncodeToString((*blockRoot)[:])
		tracing.OnStartSection("GetBeaconHeader", map[string]string{})
		header, err := beaconClient.GetBeaconHeader(ctx, headerBlock)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to fetch beacon header (%s): %w", headerBlock, err)
		}
		tracing.OnEndSection()

		beaconStateId = strconv.FormatUint(uint64(header.Header.Message.Slot), 10)
	} else {
		beaconStateId = "head"
	}

	tracing.OnStartSection("GetBeaconState", map[string]string{})
	beaconState, err := beaconClient.GetBeaconState(ctx, beaconStateId)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to fetch beacon state: %w", err)
	}
	tracing.OnEndSection()

	return checkpointTimestamp, beaconState, nil
}

func SortByStatus(validators map[string]Validator) ([]Validator, []Validator, []Validator, []Validator) {
	var awaitingActivationQueueValidators, inactiveValidators, activeValidators, withdrawnValidators []Validator

	// Iterate over all `validators` and sort them into inactive, active, or withdrawn.
	for _, validator := range validators {
		switch validator.Status {
		case ValidatorStatusInactive:
			if validator.IsAwaitingActivationQueue {
				awaitingActivationQueueValidators = append(awaitingActivationQueueValidators, validator)
			} else {
				inactiveValidators = append(inactiveValidators, validator)
			}
		case ValidatorStatusActive:
			activeValidators = append(activeValidators, validator)
		case ValidatorStatusWithdrawn:
			withdrawnValidators = append(withdrawnValidators, validator)
		}
	}

	// Sort each of these mappings in order of ascending Validator.Index
	sort.Slice(inactiveValidators, func(i, j int) bool {
		return inactiveValidators[i].Index < inactiveValidators[j].Index
	})
	sort.Slice(activeValidators, func(i, j int) bool {
		return activeValidators[i].Index < activeValidators[j].Index
	})
	sort.Slice(withdrawnValidators, func(i, j int) bool {
		return withdrawnValidators[i].Index < withdrawnValidators[j].Index
	})

	return awaitingActivationQueueValidators, inactiveValidators, activeValidators, withdrawnValidators
}

// search through beacon state for validators whose withdrawal address is set to eigenpod.
func FindAllValidatorsForEigenpod(eigenpodAddress string, beaconState *spec.VersionedBeaconState) ([]ValidatorWithIndex, error) {
	allValidators, err := beaconState.Validators()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch beacon state: %w", err)
	}

	eigenpodAddressBytes := common.FromHex(eigenpodAddress)

	var outputValidators []ValidatorWithIndex = []ValidatorWithIndex{}
	var i uint64 = 0
	maxValidators := uint64(len(allValidators))
	for i = 0; i < maxValidators; i++ {
		validator := allValidators[i]
		if validator == nil || validator.WithdrawalCredentials[0] != 1 { // withdrawalCredentials _need_ their first byte set to 1 to withdraw to execution layer.
			continue
		}
		// we check that the last 20 bytes of expectedCredentials matches validatorCredentials.
		if bytes.Equal(
			eigenpodAddressBytes,
			validator.WithdrawalCredentials[12:], // first 12 bytes are not the pubKeyHash, see (https://github.com/Layr-Labs/eigenlayer-contracts/blob/d148952a2942a97a218a2ab70f9b9f1792796081/src/contracts/pods/EigenPod.sol#L663)
		) {
			outputValidators = append(outputValidators, ValidatorWithIndex{
				Validator: validator,
				Index:     i,
			})
		}
	}
	return outputValidators, nil
}

func GetOnchainValidatorInfo(client *ethclient.Client, eigenpodAddress string, allValidators []ValidatorWithIndex) ([]onchain.IEigenPodValidatorInfo, error) {
	eigenPod, err := onchain.NewEigenPod(common.HexToAddress(eigenpodAddress), client)
	if err != nil {
		return nil, fmt.Errorf("failed to locate Eigenpod. Is your address correct?: %w", err)
	}

	var validatorInfo []onchain.IEigenPodValidatorInfo = []onchain.IEigenPodValidatorInfo{}

	// TODO: batch/multicall
	zeroes := [16]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for i := 0; i < len(allValidators); i++ {
		// ssz requires values to be 32-byte aligned, which requires 16 bytes of 0's to be added
		// prior to hashing.
		pubKeyHash := sha256.Sum256(
			append(
				(allValidators[i]).Validator.PublicKey[:],
				zeroes[:]...,
			),
		)
		info, err := eigenPod.ValidatorPubkeyHashToInfo(nil, pubKeyHash)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch validator eigeninfo: %w", err)
		}
		validatorInfo = append(validatorInfo, info)
	}

	return validatorInfo, nil
}

func GetCurrentCheckpointBlockRoot(eigenpodAddress string, eth *ethclient.Client) (*[32]byte, error) {
	eigenPod, err := onchain.NewEigenPod(common.HexToAddress(eigenpodAddress), eth)
	if err != nil {
		return nil, fmt.Errorf("failed to locate Eigenpod. Is your address correct?: %w", err)
	}

	checkpoint, err := eigenPod.CurrentCheckpoint(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to reach eigenpod: %w", err)
	}

	return &checkpoint.BeaconBlockRoot, nil
}

func IsAwaitingWithdrawalCredentialProof(validatorInfo onchain.IEigenPodValidatorInfo, validator *phase0.Validator) bool {
	return (validatorInfo.Status == ValidatorStatusInactive) && validator.ExitEpoch == FAR_FUTURE_EPOCH && validator.ActivationEpoch != FAR_FUTURE_EPOCH
}

func GetClients(ctx context.Context, node, beaconNodeUri string, enableLogs bool) (*ethclient.Client, BeaconClient, *big.Int, error) {
	eth, err := ethclient.Dial(node)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to reach eth --node: %w", err)
	}

	chainId, err := eth.ChainID(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to fetch chain id: %w", err)
	}

	if chainId == nil || (chainId.Int64() != 17000 && chainId.Int64() != 1) {
		return nil, nil, nil, errors.New("this tool only supports the Holesky and Mainnet Ethereum Networks")
	}

	beaconClient, err := GetBeaconClient(beaconNodeUri, enableLogs)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to reach beacon client: %w", err)
	}

	return eth, beaconClient, chainId, nil
}

func CastBalanceProofs(proofs []*eigenpodproofs.BalanceProof) []onchain.BeaconChainProofsBalanceProof {
	out := []onchain.BeaconChainProofsBalanceProof{}

	for i := 0; i < len(proofs); i++ {
		proof := proofs[i]
		out = append(out, onchain.BeaconChainProofsBalanceProof{
			PubkeyHash:  proof.PubkeyHash,
			BalanceRoot: proof.BalanceRoot,
			Proof:       proof.Proof.ToByteSlice(),
		})
	}

	return out
}

func PanicIfNoConsent(prompt string) {
	color.New(color.Bold).Printf("%s - Do you want to proceed? (y/n): ", prompt)
	var reply string

	fmt.Scanln(&reply)
	if reply == "y" {
		return
	} else {
		Panic("abort.")
	}
}

func PrepareAccount(owner *string, chainID *big.Int, noSend bool) (*Owner, error) {
	if noSend {
		isSimulatingGas := owner != nil && *owner != ""
		var senderPk = func() string {
			if owner == nil || *owner == "" {
				return "372d94b8645091147a5dfc10a454d0d539773d2431293bf0a195b44fa5ddbb33" // this is a RANDOM private key. Do not use this for anything.
			}
			return *owner
		}()

		privateKey, err := crypto.HexToECDSA(senderPk)
		if err != nil {
			return nil, err
		}

		publicKey := privateKey.Public()
		publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
		if !ok {
			log.Fatal("error casting public key to ECDSA")
		}
		fromAddress := crypto.PubkeyToAddress(*publicKeyECDSA)
		auth, err := bind.NewKeyedTransactorWithChainID(privateKey, chainID)
		if err != nil {
			return nil, err
		}

		if !isSimulatingGas {
			auth.GasPrice = nil             // big.NewInt(10)  // Gas price to use for the transaction execution (nil = gas price oracle)
			auth.GasFeeCap = big.NewInt(10) // big.NewInt(10) // Gas fee cap to use for the 1559 transaction execution (nil = gas price oracle)
			auth.GasTipCap = big.NewInt(2)  // big.NewInt(2) // Gas priority fee cap to use for the 1559 transaction execution (nil = gas price oracle)
			auth.GasLimit = 21000
		}
		auth.NoSend = true

		return &Owner{
			FromAddress:        fromAddress,
			PublicKey:          nil,
			TransactionOptions: auth,
			IsDryRun:           true,
		}, nil
	}

	if owner == nil {
		return nil, errors.New("no owner")
	}

	privateKey, err := crypto.HexToECDSA(*owner)
	if err != nil {
		return nil, err
	}

	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		log.Fatal("error casting public key to ECDSA")
	}
	fromAddress := crypto.PubkeyToAddress(*publicKeyECDSA)
	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, chainID)
	if err != nil {
		return nil, err
	}

	return &Owner{
		FromAddress:        fromAddress,
		PublicKey:          publicKeyECDSA,
		TransactionOptions: auth,
		IsDryRun:           noSend,
	}, nil
}

// golang was a mistake. these types are literally identical :'(
func CastValidatorFields(proof [][]eigenpodproofs.Bytes32) [][][32]byte {
	result := make([][][32]byte, len(proof))

	for i, slice := range proof {
		result[i] = make([][32]byte, len(slice))
		for j, bytes := range slice {
			result[i][j] = bytes
		}
	}

	return result
}

func Uint64ArrayToBigIntArray(nums []uint64) []*big.Int {
	out := []*big.Int{}
	for i := 0; i < len(nums); i++ {
		bigInt := new(big.Int).SetUint64(nums[i])
		out = append(out, bigInt)
	}
	return out
}

func AllZero(s []byte) bool {
	for _, v := range s {
		if v != 0 {
			return false
		}
	}
	return true
}

func WriteOutputToFileOrStdout(output []byte, out *string) error {
	if out != nil && *out != "" {
		err := os.WriteFile(*out, output, os.ModePerm)
		if err != nil {
			return err
		}
		PanicOnError("failed to write to disk", err)
		color.Green("Wrote output to %s\n", *out)
	} else {
		fmt.Println(string(output))
	}

	return nil
}

func SelectCheckpointableValidators(
	client *ethclient.Client,
	eigenpodAddress string,
	validators []ValidatorWithIndex,
	lastCheckpoint uint64,
) ([]ValidatorWithIndex, error) {
	validatorInfos, err := GetOnchainValidatorInfo(client, eigenpodAddress, validators)
	if err != nil {
		return nil, err
	}

	var checkpointValidators = []ValidatorWithIndex{}
	for i := 0; i < len(validators); i++ {
		validator := validators[i]
		validatorInfo := validatorInfos[i]

		notCheckpointed := (validatorInfo.LastCheckpointedAt != lastCheckpoint) || (validatorInfo.LastCheckpointedAt == 0)
		isActive := validatorInfo.Status == ValidatorStatusActive

		if notCheckpointed && isActive {
			checkpointValidators = append(checkpointValidators, validator)
		}
	}
	return checkpointValidators, nil
}

// (https://github.com/Layr-Labs/eigenlayer-contracts/blob/d148952a2942a97a218a2ab70f9b9f1792796081/src/contracts/libraries/BeaconChainProofs.sol#L64)
const FAR_FUTURE_EPOCH = math.MaxUint64

// Validators whose deposits have been processed but are awaiting activation on the beacon chain
// If the validator has 32 ETH effective balance, they should
func SelectAwaitingActivationValidators(
	client *ethclient.Client,
	eigenpodAddress string,
	validators []ValidatorWithIndex,
) ([]ValidatorWithIndex, error) {
	var awaitingActivationValidators = []ValidatorWithIndex{}
	for i := 0; i < len(validators); i++ {
		validator := validators[i]

		if validator.Validator.ActivationEpoch == FAR_FUTURE_EPOCH {
			awaitingActivationValidators = append(awaitingActivationValidators, validator)
		}
	}
	return awaitingActivationValidators, nil
}

func SelectAwaitingCredentialValidators(
	client *ethclient.Client,
	eigenpodAddress string,
	validators []ValidatorWithIndex,
) ([]ValidatorWithIndex, error) {
	validatorInfos, err := GetOnchainValidatorInfo(client, eigenpodAddress, validators)
	if err != nil {
		return nil, err
	}

	var awaitingCredentialValidators = []ValidatorWithIndex{}
	for i := 0; i < len(validators); i++ {
		validator := validators[i]
		validatorInfo := validatorInfos[i]

		if (validatorInfo.Status == ValidatorStatusInactive) &&
			(validator.Validator.ExitEpoch == FAR_FUTURE_EPOCH) &&
			(validator.Validator.ActivationEpoch != FAR_FUTURE_EPOCH) {
			awaitingCredentialValidators = append(awaitingCredentialValidators, validator)
		}
	}
	return awaitingCredentialValidators, nil
}

func SelectActiveValidators(
	client *ethclient.Client,
	eigenpodAddress string,
	validators []ValidatorWithIndex,
) ([]ValidatorWithIndex, error) {
	validatorInfos, err := GetOnchainValidatorInfo(client, eigenpodAddress, validators)
	if err != nil {
		return nil, err
	}

	var activeValidators = []ValidatorWithIndex{}
	for i := 0; i < len(validators); i++ {
		validator := validators[i]
		validatorInfo := validatorInfos[i]

		if validatorInfo.Status == ValidatorStatusActive {
			activeValidators = append(activeValidators, validator)
		}
	}
	return activeValidators, nil
}
