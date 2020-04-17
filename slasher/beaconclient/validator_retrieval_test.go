package beaconclient

import (
	"context"
	"reflect"
	"testing"

	"github.com/golang/mock/gomock"
	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/shared/mock"
	"github.com/prysmaticlabs/prysm/shared/testutil"
	"github.com/prysmaticlabs/prysm/slasher/cache"
	"github.com/sirupsen/logrus"
	logTest "github.com/sirupsen/logrus/hooks/test"
)

func TestService_RequestValidator(t *testing.T) {
	hook := logTest.NewGlobal()
	logrus.SetLevel(logrus.TraceLevel)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock.NewMockBeaconChainClient(ctrl)
	validatorCache, err := cache.NewPublicKeyCache(0, nil)
	if err != nil {
		t.Fatalf("could not create new cache: %v", err)
	}
	bs := Service{
		beaconClient:   client,
		publicKeyCache: validatorCache,
	}
	input1 := &ethpb.Validators{
		ValidatorList: []*ethpb.Validators_ValidatorContainer{
			{
				Index: 0, Validator: &ethpb.Validator{PublicKey: []byte{1, 2, 3}, ActivationEpoch: 1},
			},
			{
				Index: 1, Validator: &ethpb.Validator{PublicKey: []byte{2, 4, 5}, ActivationEpoch: 55000},
			},
		},
	}
	input2 := &ethpb.Validators{
		ValidatorList: []*ethpb.Validators_ValidatorContainer{
			{
				Index: 3, Validator: &ethpb.Validator{PublicKey: []byte{3, 4, 5}, ActivationEpoch: 222},
			},
		},
	}
	client.EXPECT().ListValidators(
		gomock.Any(),
		gomock.Any(),
	).Return(input1, nil)

	client.EXPECT().ListValidators(
		gomock.Any(),
		gomock.Any(),
	).Return(input2, nil)

	// We request public key of validator id 0,1.
	res, err := bs.FindOrGetValidatorsData(context.Background(), []uint64{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range input1.ValidatorList {
		wanted := cache.ValidatorData{
			PublicKey:       input1.ValidatorList[i].Validator.PublicKey,
			ActivationEpoch: input1.ValidatorList[i].Validator.ActivationEpoch,
		}
		if !reflect.DeepEqual(res[v.Index], wanted) {
			t.Errorf("Wanted %v, received %v", input1, res)
		}
	}

	testutil.AssertLogsContain(t, hook, "Retrieved validators id public key map:")
	testutil.AssertLogsDoNotContain(t, hook, "Retrieved validators public keys from cache:")
	// We expect public key of validator id 0 to be in cache.
	res, err = bs.FindOrGetValidatorsData(context.Background(), []uint64{0, 3})
	if err != nil {
		t.Fatal(err)
	}

	for i, v := range input2.ValidatorList {
		wanted := cache.ValidatorData{
			PublicKey:       input2.ValidatorList[i].Validator.PublicKey,
			ActivationEpoch: input2.ValidatorList[i].Validator.ActivationEpoch,
		}
		if !reflect.DeepEqual(res[v.Index], wanted) {
			t.Errorf("Wanted %v, received %v", input2, res)
		}
	}
	testutil.AssertLogsContain(t, hook, "Retrieved validators public keys from cache:")
}
