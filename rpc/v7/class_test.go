package rpcv7_test

import (
	"context"
	"errors"
	"testing"

	"github.com/NethermindEth/juno/clients/feeder"
	"github.com/NethermindEth/juno/core"
	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/juno/db"
	"github.com/NethermindEth/juno/mocks"
	"github.com/NethermindEth/juno/rpc/rpccore"
	rpcv7 "github.com/NethermindEth/juno/rpc/v7"
	adaptfeeder "github.com/NethermindEth/juno/starknetdata/feeder"
	"github.com/NethermindEth/juno/utils"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestClass(t *testing.T) {
	n := utils.Ptr(utils.Integration)
	integrationClient := feeder.NewTestClient(t, n)
	integGw := adaptfeeder.New(integrationClient)

	mockCtrl := gomock.NewController(t)
	t.Cleanup(mockCtrl.Finish)

	mockReader := mocks.NewMockReader(mockCtrl)
	mockState := mocks.NewMockStateHistoryReader(mockCtrl)

	mockState.EXPECT().Class(gomock.Any()).DoAndReturn(func(classHash *felt.Felt) (*core.DeclaredClass, error) {
		class, err := integGw.Class(context.Background(), classHash)
		return &core.DeclaredClass{Class: class, At: 0}, err
	}).AnyTimes()
	mockReader.EXPECT().HeadState().Return(mockState, func() error {
		return nil
	}, nil).AnyTimes()
	mockReader.EXPECT().HeadsHeader().Return(new(core.Header), nil).AnyTimes()
	handler := rpcv7.New(mockReader, nil, nil, "", n, utils.NewNopZapLogger())

	latest := rpcv7.BlockID{Latest: true}

	t.Run("sierra class", func(t *testing.T) {
		hash := utils.HexToFelt(t, "0x1cd2edfb485241c4403254d550de0a097fa76743cd30696f714a491a454bad5")

		coreClass, err := integGw.Class(context.Background(), hash)
		require.NoError(t, err)

		class, rpcErr := handler.Class(latest, *hash)
		require.Nil(t, rpcErr)
		cairo1Class := coreClass.(*core.Cairo1Class)
		assertEqualCairo1Class(t, cairo1Class, class)
	})

	t.Run("casm class", func(t *testing.T) {
		hash := utils.HexToFelt(t, "0x4631b6b3fa31e140524b7d21ba784cea223e618bffe60b5bbdca44a8b45be04")

		coreClass, err := integGw.Class(context.Background(), hash)
		require.NoError(t, err)

		class, rpcErr := handler.Class(latest, *hash)
		require.Nil(t, rpcErr)

		cairo0Class := coreClass.(*core.Cairo0Class)
		assertEqualCairo0Class(t, cairo0Class, class)
	})

	t.Run("state by id error", func(t *testing.T) {
		mockReader := mocks.NewMockReader(mockCtrl)
		handler := rpcv7.New(mockReader, nil, nil, "", n, utils.NewNopZapLogger())

		mockReader.EXPECT().HeadState().Return(nil, nil, db.ErrKeyNotFound)

		_, rpcErr := handler.Class(latest, felt.Zero)
		require.NotNil(t, rpcErr)
		require.Equal(t, rpccore.ErrBlockNotFound, rpcErr)
	})

	t.Run("class hash not found error", func(t *testing.T) {
		mockReader := mocks.NewMockReader(mockCtrl)
		mockState := mocks.NewMockStateHistoryReader(mockCtrl)
		handler := rpcv7.New(mockReader, nil, nil, "", n, utils.NewNopZapLogger())

		mockReader.EXPECT().HeadState().Return(mockState, func() error {
			return nil
		}, nil)
		mockState.EXPECT().Class(gomock.Any()).Return(nil, errors.New("class hash not found"))

		_, rpcErr := handler.Class(latest, felt.Zero)
		require.NotNil(t, rpcErr)
		require.Equal(t, rpccore.ErrClassHashNotFound, rpcErr)
	})
}
