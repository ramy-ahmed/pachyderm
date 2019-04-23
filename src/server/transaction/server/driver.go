package server

import (
	"fmt"
	"time"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/transaction"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/serviceenv"
	"github.com/pachyderm/pachyderm/src/server/pkg/transactiondb"
	txnenv "github.com/pachyderm/pachyderm/src/server/pkg/transactionenv"
	"github.com/pachyderm/pachyderm/src/server/pkg/uuid"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
)

type driver struct {
	// txnEnv stores references to other pachyderm APIServer instances so we can
	// make calls within the same transaction without serializing through RPCs
	txnEnv *txnenv.TransactionEnv

	// etcdClient and prefix write repo and other metadata to etcd
	etcdClient *etcd.Client
	prefix     string

	// collections
	transactions col.Collection
}

func newDriver(
	env *serviceenv.ServiceEnv,
	txnEnv *txnenv.TransactionEnv,
	etcdPrefix string,
) (*driver, error) {
	etcdClient := env.GetEtcdClient()
	d := &driver{
		txnEnv:       txnEnv,
		etcdClient:   etcdClient,
		prefix:       etcdPrefix,
		transactions: transactiondb.Transactions(etcdClient, etcdPrefix),
	}
	return d, nil
}

func now() *types.Timestamp {
	t, err := types.TimestampProto(time.Now())
	if err != nil {
		panic(err)
	}
	return t
}

func (d *driver) startTransaction(pachClient *client.APIClient) (*transaction.Transaction, error) {
	ctx := pachClient.Ctx()
	info := &transaction.TransactionInfo{
		Transaction: &transaction.Transaction{
			ID: uuid.New(),
		},
		Requests: []*transaction.TransactionRequest{},
		Started:  now(),
	}

	_, err := col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
		return d.transactions.ReadWrite(stm).Put(
			info.Transaction.ID,
			info,
		)
	})
	if err != nil {
		return nil, err
	}
	return info.Transaction, nil
}

func (d *driver) inspectTransaction(pachClient *client.APIClient, txn *transaction.Transaction) (*transaction.TransactionInfo, error) {
	ctx := pachClient.Ctx()
	info := &transaction.TransactionInfo{}
	err := d.transactions.ReadOnly(ctx).Get(txn.ID, info)
	if err != nil {
		return nil, err
	}
	return info, nil
}

func (d *driver) deleteTransaction(pachClient *client.APIClient, txn *transaction.Transaction) error {
	ctx := pachClient.Ctx()
	_, err := col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
		return d.transactions.ReadWrite(stm).Delete(txn.ID)
	})
	return err
}

func (d *driver) listTransaction(pachClient *client.APIClient) ([]*transaction.TransactionInfo, error) {
	var result []*transaction.TransactionInfo
	transactionInfo := &transaction.TransactionInfo{}
	transactions := d.transactions.ReadOnly(pachClient.Ctx())
	if err := transactions.List(transactionInfo, col.DefaultOptions, func(string) error {
		result = append(result, proto.Clone(transactionInfo).(*transaction.TransactionInfo))
		return nil
	}); err != nil {
		return nil, err
	}
	return result, nil
}

// deleteAll deletes all transactions from etcd except the currently running
// transaction (if any).
func (d *driver) deleteAll(pachClient *client.APIClient, stm col.STM, running *transaction.Transaction) error {
	txns, err := d.listTransaction(pachClient)
	if err != nil {
		return err
	}

	transactions := d.transactions.ReadWrite(stm)
	for _, info := range txns {
		if running == nil || info.Transaction.ID != running.ID {
			err := transactions.Delete(info.Transaction.ID)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *driver) runTransaction(pachClient *client.APIClient, stm col.STM, info *transaction.TransactionInfo) (*transaction.TransactionInfo, error) {
	responses := []*transaction.TransactionResponse{}
	for i, request := range info.Requests {
		var err error
		var response *transaction.TransactionResponse
		switch x := request.Request.(type) {
		case *transaction.TransactionRequest_CreateRepo:
			err = d.txnEnv.PfsServer().CreateRepoInTransaction(pachClient, stm, x.CreateRepo)
			response = client.NewEmptyResponse()
		case *transaction.TransactionRequest_DeleteRepo:
			err = d.txnEnv.PfsServer().DeleteRepoInTransaction(pachClient, stm, x.DeleteRepo)
			response = client.NewEmptyResponse()
		case *transaction.TransactionRequest_StartCommit:
			// Do a little extra work here so we can make sure the new commit ID is
			// the same every time.  We store the response the first time and reuse
			// the commit ID on subsequent runs.
			var commit *pfs.Commit
			if len(info.Responses) > i {
				switch commitResponse := info.Responses[i].Response.(type) {
				case *transaction.TransactionResponse_Commit:
					commit = commitResponse.Commit
				default:
					err = fmt.Errorf("unexpected stored response type")
				}
			}
			if err == nil {
				commit, err = d.txnEnv.PfsServer().StartCommitInTransaction(pachClient, stm, x.StartCommit, commit)
				response = client.NewCommitResponse(commit)
			}
		case *transaction.TransactionRequest_FinishCommit:
			err = d.txnEnv.PfsServer().FinishCommitInTransaction(pachClient, stm, x.FinishCommit)
			response = client.NewEmptyResponse()
		case *transaction.TransactionRequest_DeleteCommit:
			err = d.txnEnv.PfsServer().DeleteCommitInTransaction(pachClient, stm, x.DeleteCommit)
			response = client.NewEmptyResponse()
		case *transaction.TransactionRequest_CreateBranch:
			err = d.txnEnv.PfsServer().CreateBranchInTransaction(pachClient, stm, x.CreateBranch)
			response = client.NewEmptyResponse()
		case *transaction.TransactionRequest_DeleteBranch:
			err = d.txnEnv.PfsServer().DeleteBranchInTransaction(pachClient, stm, x.DeleteBranch)
			response = client.NewEmptyResponse()
		case *transaction.TransactionRequest_CopyFile:
			err = d.txnEnv.PfsServer().CopyFileInTransaction(pachClient, stm, x.CopyFile)
			response = client.NewEmptyResponse()
		case *transaction.TransactionRequest_DeleteFile:
			err = d.txnEnv.PfsServer().DeleteFileInTransaction(pachClient, stm, x.DeleteFile)
			response = client.NewEmptyResponse()
		case *transaction.TransactionRequest_DeleteAll:
			// TODO: extend this to delete everything through PFS, PPS, Auth and
			// update the client DeleteAll call to use only this, then remove unused
			// RPCs.
			err = d.deleteAll(pachClient, stm, info.Transaction)
			response = client.NewEmptyResponse()
		default:
			err = fmt.Errorf("unrecognized transaction request type")
		}

		if err != nil {
			return nil, fmt.Errorf("error running request %d of %d: %v", i+1, len(info.Requests), err)
		}

		responses = append(responses, response)
	}

	info.Responses = responses
	return info, nil
}

func (d *driver) finishTransaction(pachClient *client.APIClient, txn *transaction.Transaction) (*transaction.TransactionInfo, error) {
	ctx := pachClient.Ctx()
	info := &transaction.TransactionInfo{}
	_, err := col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
		err := d.transactions.ReadOnly(ctx).Get(txn.ID, info)
		if err != nil {
			return err
		}
		info, err = d.runTransaction(pachClient, stm, info)
		if err != nil {
			return err
		}
		return d.transactions.ReadWrite(stm).Delete(txn.ID)
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// Error to be returned when the transaction has been modified between our two STM calls
type transactionConflictError struct{}

func (e *transactionConflictError) Error() string {
	return fmt.Sprintf("transaction could not be modified due to concurrent modifications")
}

func (d *driver) appendTransaction(
	pachClient *client.APIClient,
	txn *transaction.Transaction,
	items []*transaction.TransactionRequest,
) (*transaction.TransactionInfo, error) {
	ctx := pachClient.Ctx()

	// Run this thing in a loop in case we get a conflict, time out after some tries
	for i := 0; i < 10; i++ {
		// We first do a dryrun of the transaction to
		// 1. make sure the appended request is valid
		// 2. Capture the result of the request to be returned
		var numRequests, numResponses int
		var dryrunResponses []*transaction.TransactionResponse

		info := &transaction.TransactionInfo{}
		err := col.NewDryrunSTM(ctx, d.etcdClient, func(stm col.STM) error {
			// Get the existing transaction and append the new requests
			err := d.transactions.ReadWrite(stm).Get(txn.ID, info)
			if err != nil {
				return err
			}

			// Save the length so that we can check that nothing else modifies the
			// transaction in the meantime
			numRequests = len(info.Requests)
			numResponses = len(info.Responses)
			info.Requests = append(info.Requests, items...)

			info, err = d.runTransaction(pachClient, stm, info)
			if err != nil {
				return err
			}

			dryrunResponses = info.Responses[numResponses:]
			return nil
		})

		if err != nil {
			return nil, err
		}

		info = &transaction.TransactionInfo{}
		_, err = col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
			// Update the existing transaction with the new requests/responses
			return d.transactions.ReadWrite(stm).Update(txn.ID, info, func() error {
				if len(info.Requests) != numRequests || len(info.Responses) != numResponses {
					// Someone else modified the transaction while we did the dry run
					return &transactionConflictError{}
				}

				info.Requests = append(info.Requests, items...)
				info.Responses = append(info.Responses, dryrunResponses...)
				return nil
			})
		})

		if err == nil {
			return info, nil
		}
		switch err.(type) {
		case *transactionConflictError: // pass
		default:
			return nil, err
		}
	}
	return nil, &transactionConflictError{}
}
