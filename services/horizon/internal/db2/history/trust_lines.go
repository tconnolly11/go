package history

import (
	"context"
	"database/sql"
	"encoding/base64"

	sq "github.com/Masterminds/squirrel"
	"github.com/guregu/null"
	"github.com/lib/pq"
	"github.com/stellar/go/support/errors"
	"github.com/stellar/go/xdr"
)

// IsAuthorized returns true if issuer has authorized account to perform
// transactions with its credit
func (trustLine TrustLine) IsAuthorized() bool {
	return xdr.TrustLineFlags(trustLine.Flags).IsAuthorized()
}

// IsAuthorizedToMaintainLiabilities returns true if issuer has authorized the account to maintain
// liabilities with its credit
func (trustLine TrustLine) IsAuthorizedToMaintainLiabilities() bool {
	return xdr.TrustLineFlags(trustLine.Flags).IsAuthorizedToMaintainLiabilitiesFlag()
}

// IsClawbackEnabled returns true if issuer has authorized the account to claw
// assets back
func (trustLine TrustLine) IsClawbackEnabled() bool {
	return xdr.TrustLineFlags(trustLine.Flags).IsClawbackEnabledFlag()
}

// AssetsForAddress returns a list of assets and balances for those assets held by
// a given address.
func (q *Q) AssetsForAddress(ctx context.Context, addy string) ([]xdr.Asset, []xdr.Int64, error) {
	if tx := q.GetTx(); tx == nil {
		return nil, nil, errors.New("cannot be called outside of a transaction")
	}
	if opts := q.GetTxOptions(); opts == nil || !opts.ReadOnly || opts.Isolation != sql.LevelRepeatableRead {
		return nil, nil, errors.New("should only be called in a repeatable read transaction")
	}

	account, err := q.GetAccountByID(ctx, addy)

	if q.NoRows(err) {
		// if there is no account for the given address then
		// we return an empty list of assets and balances
		return []xdr.Asset{}, []xdr.Int64{}, nil
	} else if err != nil {
		return nil, nil, err
	}

	var tls []TrustLine
	err = q.Select(ctx, &tls, selectTrustLines.Where(sq.Eq{"account_id": addy}))
	if err != nil {
		return nil, nil, err
	}

	assets := make([]xdr.Asset, len(tls)+1)
	balances := make([]xdr.Int64, len(tls)+1)
	for i, tl := range tls {
		assets[i] = xdr.MustNewCreditAsset(tl.AssetCode, tl.AssetIssuer)
		balances[i] = xdr.Int64(tl.Balance)
	}

	assets[len(assets)-1] = xdr.MustNewNativeAsset()
	balances[len(assets)-1] = xdr.Int64(account.Balance)

	return assets, balances, err
}

func (q *Q) CountTrustLines(ctx context.Context) (int, error) {
	sql := sq.Select("count(*)").From("trust_lines")

	var count int
	if err := q.Get(ctx, &count, sql); err != nil {
		return 0, errors.Wrap(err, "could not run select query")
	}

	return count, nil
}

func (q *Q) GetSortedTrustLinesByAccountID(ctx context.Context, id string) ([]TrustLine, error) {
	return q.GetSortedTrustLinesByAccountIDs(ctx, []string{id})
}

// GetTrustLinesByKeys loads a row from the `trust_lines` table, selected by multiple keys.
func (q *Q) GetTrustLinesByKeys(ctx context.Context, keys []xdr.LedgerKeyTrustLine) ([]TrustLine, error) {
	var trustLines []TrustLine
	lkeys := make([]string, 0, len(keys))
	for _, key := range keys {
		lkey, err := ledgerKeyTrustLineToString(key)
		if err != nil {
			return nil, errors.Wrap(err, "Error running ledgerKeyTrustLineToString")
		}
		lkeys = append(lkeys, lkey)
	}
	sql := selectTrustLines.Where(map[string]interface{}{"trust_lines.ledger_key": lkeys})
	err := q.Select(ctx, &trustLines, sql)
	return trustLines, err
}

// InsertTrustLine creates a row in the trust lines table.
// Returns number of rows affected and error.
func (q *Q) InsertTrustLine(ctx context.Context, entry xdr.LedgerEntry) (int64, error) {
	m := trustLineToMap(entry)

	// Add lkey only when inserting rows
	key, err := trustLineEntryToLedgerKeyString(entry)
	if err != nil {
		return 0, errors.Wrap(err, "Error running trustLineEntryToLedgerKeyString")
	}
	m["ledger_key"] = key

	sql := sq.Insert("trust_lines").SetMap(m)
	result, err := q.Exec(ctx, sql)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

// UpdateTrustLine updates a row in the trust lines table.
// Returns number of rows affected and error.
func (q *Q) UpdateTrustLine(ctx context.Context, entry xdr.LedgerEntry) (int64, error) {
	key, err := trustLineEntryToLedgerKeyString(entry)
	if err != nil {
		return 0, errors.Wrap(err, "Error running trustLineEntryToLedgerKeyString")
	}

	sql := sq.Update("trust_lines").
		SetMap(trustLineToMap(entry)).
		Where(map[string]interface{}{"ledger_key": key})
	result, err := q.Exec(ctx, sql)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

// UpsertTrustLines upserts a batch of trust lines in the trust lines table.
// There's currently no limit of the number of trust lines this method can
// accept other than 2GB limit of the query string length what should be enough
// for each ledger with the current limits.
func (q *Q) UpsertTrustLines(ctx context.Context, trustLines []xdr.LedgerEntry) error {
	var ledgerKey, accountID, assetIssuer, assetCode []string
	var balance, limit, buyingLiabilities, sellingLiabilities []xdr.Int64
	var flags, lastModifiedLedger []xdr.Uint32
	var assetType []xdr.AssetType
	var sponsor []null.String

	for _, entry := range trustLines {
		if entry.Data.Type != xdr.LedgerEntryTypeTrustline {
			return errors.Errorf("Invalid entry type: %d", entry.Data.Type)
		}

		key, err := trustLineEntryToLedgerKeyString(entry)
		if err != nil {
			return errors.Wrap(err, "Error running trustLineEntryToLedgerKeyString")
		}

		m := trustLineToMap(entry)
		ledgerKey = append(ledgerKey, key)
		accountID = append(accountID, m["account_id"].(string))
		assetType = append(assetType, m["asset_type"].(xdr.AssetType))
		assetIssuer = append(assetIssuer, m["asset_issuer"].(string))
		assetCode = append(assetCode, m["asset_code"].(string))
		balance = append(balance, m["balance"].(xdr.Int64))
		limit = append(limit, m["trust_line_limit"].(xdr.Int64))
		buyingLiabilities = append(buyingLiabilities, m["buying_liabilities"].(xdr.Int64))
		sellingLiabilities = append(sellingLiabilities, m["selling_liabilities"].(xdr.Int64))
		flags = append(flags, m["flags"].(xdr.Uint32))
		lastModifiedLedger = append(lastModifiedLedger, m["last_modified_ledger"].(xdr.Uint32))
		sponsor = append(sponsor, m["sponsor"].(null.String))
	}

	sql := `
	WITH r AS
		(SELECT
			unnest(?::text[]),
			unnest(?::text[]),
			unnest(?::int[]),
			unnest(?::text[]),
			unnest(?::text[]),
			unnest(?::bigint[]),
			unnest(?::bigint[]),
			unnest(?::bigint[]),
			unnest(?::bigint[]),
			unnest(?::int[]),
			unnest(?::int[]),
			unnest(?::text[])
		)
	INSERT INTO trust_lines ( 
		ledger_key,
		account_id,
		asset_type,
		asset_issuer,
		asset_code,
		balance,
		trust_line_limit,
		buying_liabilities,
		selling_liabilities,
		flags,
		last_modified_ledger,
		sponsor
	)
	SELECT * from r 
	ON CONFLICT (ledger_key) DO UPDATE SET 
		ledger_key = excluded.ledger_key,
		account_id = excluded.account_id,
		asset_type = excluded.asset_type,
		asset_issuer = excluded.asset_issuer,
		asset_code = excluded.asset_code,
		balance = excluded.balance,
		trust_line_limit = excluded.trust_line_limit,
		buying_liabilities = excluded.buying_liabilities,
		selling_liabilities = excluded.selling_liabilities,
		flags = excluded.flags,
		last_modified_ledger = excluded.last_modified_ledger,
		sponsor = excluded.sponsor`

	_, err := q.ExecRaw(ctx, sql,
		pq.Array(ledgerKey),
		pq.Array(accountID),
		pq.Array(assetType),
		pq.Array(assetIssuer),
		pq.Array(assetCode),
		pq.Array(balance),
		pq.Array(limit),
		pq.Array(buyingLiabilities),
		pq.Array(sellingLiabilities),
		pq.Array(flags),
		pq.Array(lastModifiedLedger),
		pq.Array(sponsor))
	return err
}

// RemoveTrustLine deletes a row in the trust lines table.
// Returns number of rows affected and error.
func (q *Q) RemoveTrustLine(ctx context.Context, ledgerKey xdr.LedgerKeyTrustLine) (int64, error) {
	key, err := ledgerKeyTrustLineToString(ledgerKey)
	if err != nil {
		return 0, errors.Wrap(err, "Error ledgerKeyTrustLineToString MarshalBinaryCompress")
	}

	sql := sq.Delete("trust_lines").
		Where(map[string]interface{}{"ledger_key": key})
	result, err := q.Exec(ctx, sql)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

// GetSortedTrustLinesByAccountIDs loads trust lines for a list of accounts ID, ordered by asset and issuer
func (q *Q) GetSortedTrustLinesByAccountIDs(ctx context.Context, id []string) ([]TrustLine, error) {
	var data []TrustLine
	sql := selectTrustLines.Where(sq.Eq{"account_id": id}).OrderBy("asset_code", "asset_issuer")
	err := q.Select(ctx, &data, sql)
	return data, err
}

func trustLineEntryToLedgerKeyString(entry xdr.LedgerEntry) (string, error) {
	ledgerKey := entry.LedgerKey()
	key, err := ledgerKey.MarshalBinary()
	if err != nil {
		return "", errors.Wrap(err, "Error running MarshalBinaryCompress")
	}

	return base64.StdEncoding.EncodeToString(key), nil
}

func ledgerKeyTrustLineToString(trustLineKey xdr.LedgerKeyTrustLine) (string, error) {
	ledgerKey := &xdr.LedgerKey{}
	err := ledgerKey.SetTrustline(trustLineKey.AccountId, trustLineKey.Asset)
	if err != nil {
		return "", errors.Wrap(err, "Error running ledgerKey.SetTrustline")
	}
	key, err := ledgerKey.MarshalBinary()
	if err != nil {
		return "", errors.Wrap(err, "Error running MarshalBinaryCompress")
	}

	return base64.StdEncoding.EncodeToString(key), nil
}

func trustLineToMap(entry xdr.LedgerEntry) map[string]interface{} {
	trustLine := entry.Data.MustTrustLine()

	var assetType xdr.AssetType
	var assetCode, assetIssuer string
	trustLine.Asset.MustExtract(&assetType, &assetCode, &assetIssuer)

	liabilities := trustLine.Liabilities()
	return map[string]interface{}{
		"account_id":           trustLine.AccountId.Address(),
		"asset_type":           assetType,
		"asset_issuer":         assetIssuer,
		"asset_code":           assetCode,
		"balance":              trustLine.Balance,
		"trust_line_limit":     trustLine.Limit,
		"buying_liabilities":   liabilities.Buying,
		"selling_liabilities":  liabilities.Selling,
		"flags":                trustLine.Flags,
		"last_modified_ledger": entry.LastModifiedLedgerSeq,
		"sponsor":              ledgerEntrySponsorToNullString(entry),
	}
}

var selectTrustLines = sq.Select(`
	account_id,
	asset_type,
	asset_issuer,
	asset_code,
	balance,
	trust_line_limit,
	buying_liabilities,
	selling_liabilities,
	flags,
	last_modified_ledger,
	sponsor
`).From("trust_lines")
