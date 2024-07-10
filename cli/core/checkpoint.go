package core

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strconv"

	eigenpodproofs "github.com/Layr-Labs/eigenpod-proofs-generation"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fatih/color"
)

// , out, owner *string, forceCheckpoint bool
func LoadCheckpointProofFromFile(path string) (*eigenpodproofs.VerifyCheckpointProofsCallParams, error) {
	res := eigenpodproofs.VerifyCheckpointProofsCallParams{}
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(bytes, &res)
	if err != nil {
		return nil, err
	}

	return &res, nil
}

func GenerateCheckpointProof(ctx context.Context, eigenpodAddress string, eth *ethclient.Client, chainId *big.Int, beaconClient BeaconClient) *eigenpodproofs.VerifyCheckpointProofsCallParams {
	currentCheckpoint := GetCurrentCheckpoint(eigenpodAddress, eth)
	blockRoot, err := GetCurrentCheckpointBlockRoot(eigenpodAddress, eth)
	PanicOnError("failed to fetch last checkpoint.", err)
	if blockRoot == nil {
		Panic("failed to fetch last checkpoint - nil blockRoot")
	}

	if blockRoot != nil {
		rootBytes := *blockRoot
		if AllZero(rootBytes[:]) {
			PanicOnError("No checkpoint active. Are you sure you started a checkpoint?", fmt.Errorf("no checkpoint"))
		}
	}

	headerBlock := "0x" + hex.EncodeToString((*blockRoot)[:])
	header, err := beaconClient.GetBeaconHeader(ctx, headerBlock)
	PanicOnError(fmt.Sprintf("failed to fetch beacon header (%s).", headerBlock), err)

	beaconState, err := beaconClient.GetBeaconState(ctx, strconv.FormatUint(uint64(header.Header.Message.Slot), 10))
	PanicOnError("failed to fetch beacon state.", err)

	// filter through the beaconState's validators, and select only ones that have withdrawal address set to `eigenpod`.
	allValidators := FindAllValidatorsForEigenpod(eigenpodAddress, beaconState)
	color.Yellow("You have a total of %d validators pointed to this pod.", len(allValidators))

	checkpointValidators := SelectCheckpointableValidators(eth, eigenpodAddress, allValidators, currentCheckpoint)
	validatorIndices := make([]uint64, len(checkpointValidators))
	for i, v := range checkpointValidators {
		validatorIndices[i] = v.Index
	}

	color.Yellow("Proving validators at indices: %s", validatorIndices)

	proofs, err := eigenpodproofs.NewEigenPodProofs(chainId.Uint64(), 300 /* oracleStateCacheExpirySeconds - 5min */)
	PanicOnError("failled to initialize prover", err)

	proof, err := proofs.ProveCheckpointProofs(header.Header.Message, beaconState, validatorIndices)
	PanicOnError("failed to prove checkpoint.", err)

	return proof
}