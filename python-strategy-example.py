import yfinance as yf
import pandas as pd
import numpy as np
import matplotlib.pyplot as plt
import os, time, requests
from datetime import datetime
import json
import random


# global cache so we don't re-download fundamentals for same cutoff
FUNDAMENTAL_CACHE = {}

def load_sf1_full(tickers):
    """
    Downloads ALL PIT (‘ART’) rows for ALL tickers in one request.
    Cached locally to avoid repeated API calls.
    """
    global SF1_FULL_CACHE

    if 'SF1_FULL_CACHE' in globals():
        return SF1_FULL_CACHE.copy()

    print("Downloading full SF1 dataset (this can take 15–30 seconds)...")

    params = {
        "ticker": ",".join(tickers),
        "dimension": "ART",
        "qopts.columns": (
            "ticker,dimension,datekey,ev,ebit,fcf,marketcap,gp,assets,roic,netinc,revenue"
        ),
        "api_key": API_KEY
    }

    url = "https://data.nasdaq.com/api/v3/datatables/SHARADAR/SF1"
    js = fetch_json_with_retry(url, params=params)

    cols = [c["name"] for c in js["datatable"]["columns"]]
    df = pd.DataFrame(js["datatable"]["data"], columns=cols)

    df["datekey"] = pd.to_datetime(df["datekey"], errors="coerce")

    SF1_FULL_CACHE = df.copy()
    print(f"Loaded {len(df)} SF1 rows.")
    return df


def fetch_json_with_retry(url, params, retries=20, base_wait=1.0):
    """Robust fetch with rate-limit detection, 429 handling, exponential backoff and jitter."""
    for attempt in range(retries):
        r = requests.get(url, params=params)

        # Rate limited
        if r.status_code == 429:
            wait = base_wait * (2 ** attempt) + random.random()
            print(f"  429 rate limit — waiting {wait:.1f}s...")
            time.sleep(wait)
            continue

        # Server failure
        if r.status_code >= 500:
            wait = base_wait * (2 ** attempt) + random.random()
            print(f"  Server error {r.status_code} — waiting {wait:.1f}s...")
            time.sleep(wait)
            continue

        try:
            return r.json()
        except json.JSONDecodeError:
            wait = base_wait * (2 ** attempt) + random.random()
            print(f"  JSON decode failed (attempt {attempt+1}/{retries}), waiting {wait:.1f}s...")
            time.sleep(wait)

    raise RuntimeError("Sharadar API failed too many times — likely daily rate limit reached.")



# ============================================
# CONFIG
# ============================================

SNAP = pd.Timestamp("2015-07-01") #SNAP = pd.Timestamp("2015-07-01")
END = pd.Timestamp("2025-12-09")
N_STOCKS = 6
LAG_DAYS = 60

SP500_CLEAN_PATH = "/kaggle/input/sp500-clean/sp500_clean.csv"
SF1_URL = "https://data.nasdaq.com/api/v3/datatables/SHARADAR/SF1"
API_KEY = "NnrqMcysLSoCm6nkgk_G"


# ============================================
# 1) LOAD UNIVERSE
# ============================================

def load_sp500(path):
    df = pd.read_csv(path)
    tickers = df["Symbol"].dropna().astype(str).unique().tolist()
    print(f"Loaded {len(tickers)} tickers")
    return tickers

# ============================================
# 1B) LOAD SHARADAR S&P500 MEMBERSHIP (PIT)
# ============================================

def load_sp500_membership():
    """
    Loads PIT S&P500 membership from Sharadar.
    This file contains columns:
        ticker, date, action
    Where action is 'add' or 'remove'.
    """
    path = "/kaggle/input/sharadar/sp500.csv"   # <-- adjust to your dataset name
    df = pd.read_csv(path)
    df["date"] = pd.to_datetime(df["date"])
    return df



# ============================================
# 2) LOAD PRICE DATA
# ============================================

def load_prices(tickers, snap, end):
    print("Downloading prices...")

    start_for_prices = (snap - pd.Timedelta(days=366)).strftime("%Y-%m-%d")

    raw = yf.download(
        tickers,
        start=start_for_prices,
        end=end + pd.Timedelta(days=1),
        auto_adjust=True,
        progress=False
    )

    # Extract only Close prices
    px = raw["Close"]

    # Drop tickers with no price data at all
    px = px.dropna(axis=1, how="all")

    print(f"Prices downloaded for {len(px.columns)} tickers, {len(px)} rows.")

    # First valid trading day ≥ SNAP
    buy_idx = px.index.searchsorted(snap)
    buy_dt = px.index[buy_idx]

    print("Buy date:", buy_dt.date())
    return px, buy_dt



# ============================================
# 3) FUNDAMENTALS (PIT ART)
# ============================================

def load_fundamentals_pit(full_sf1, snap_date, lag_days=LAG_DAYS):
    cutoff = snap_date - pd.Timedelta(days=lag_days)

    df = full_sf1[ full_sf1["datekey"] <= cutoff ].copy()
    df = df.sort_values(["ticker","datekey"]).groupby("ticker").tail(1)
    df = df.set_index("ticker")
    return df


# ============================================
# 3B) GET MEMBERSHIP FOR A SPECIFIC DATE
# ============================================

def sp500_members_at_date(df, date):
    """Return all tickers that were members of SP500 on a given date."""

    # For each ticker, find its "entry" and "exit" boundaries
    records = []

    for tkr, grp in df.sort_values("date").groupby("ticker"):
        adds  = grp[grp["action"] == "add"]["date"]
        drops = grp[grp["action"] == "drop"]["date"]

        if len(adds):
            entry = adds.min()
        else:
            entry = pd.Timestamp("1900-01-01")
        if len(drops):
            exit = drops.min()
        else:
            exit = pd.Timestamp("2100-01-01")

        records.append((tkr, entry, exit))

    hist = pd.DataFrame(records, columns=["ticker", "entry", "exit"])

    # Select tickers active at the given date
    members = hist[(hist["entry"] <= date) & (date < hist["exit"])]["ticker"].tolist()
    return members


# ============================================
# 4) FACTORS
# ============================================

def compute_factors(df):
    df = df.copy()

    df["roe"] = df["netinc"] / df["assets"]
    df["ev_to_ebit"] = df["ev"] / df["ebit"]
    df["gp_a"] = df["gp"] / df["assets"]
    df["accruals"] = df["fcf"] / df["netinc"]

    needed = ["ev_to_ebit", "roe", "gp_a"]
    df = df.dropna(subset=needed)

    return df

# ============================================
# 5) STOCK SELECTION (Value + Quality)
# ============================================

def select_stocks(df_clean, n, verbose=True, min_keep=N_STOCKS):
    df_all = df_clean.copy()

    df = df_all.copy()   # keep all ROE values

    # -----------------------------
    # 2) Quality + value filters
    # -----------------------------
    df = df[df["roe"] >= 0.12]                     # high capital efficiency
    df = df[df["gp_a"] >= df["gp_a"].median()]     # high operating quality
    df = df[df["ev_to_ebit"].between(0, 25)]       # reasonably priced

    # -----------------------------
    # 3) Ranking (apply to full df_all)
    # -----------------------------
    df_all["rank"] = (
        0.6 * df_all["ev_to_ebit"].rank(ascending=True) +
        0.4 * df_all["roe"].rank(ascending=False)
    )

    df_all = df_all.sort_values("rank")

    # -----------------------------
    # 4) If too few stocks survive filters:
    #    Keep what survived + fill remainder from best-ranked non-survivors
    # -----------------------------
    survivors = df.index.tolist()

    if len(survivors) < min_keep:
        needed = min_keep - len(survivors)

        # pick the next-best ranked stocks not in survivors
        fill = [t for t in df_all.index if t not in survivors][:needed]
        survivors.extend(fill)

        if verbose:
            print(f"WARNING: Only {len(df)} passed filters. Filled with {len(fill)} fallback stocks.")

    # -----------------------------
    # 5) Now take top N from final ordering
    # -----------------------------
    # IMPORTANT: This is the bug-fix line
    final_df = df_all.loc[df_all.index.intersection(survivors)]

    picks = final_df.head(n).index.tolist()

    return picks


# ============================================
# 6) REBALANCING ENGINE
# ============================================

def quarterly_rebalance(tickers, px, rebalance_dates, n=20):
    portfolios = []
    prev_holdings = []

    for date in rebalance_dates:
        print("\n=== REBAL:", date.date(), "===")

        fundas = load_fundamentals_pit(tickers, date)
        df_clean = compute_factors(fundas)

        new_holdings = select_stocks(df_clean, n, verbose=False)
        portfolios.append((date, new_holdings))

        print("Holdings:", new_holdings)

    return portfolios


# ============================================
# 7) REBALANCED PORTFOLIO CURVE
# ============================================

def compute_rebalanced_curve(px, portfolios, end_date):
    """
    Build a correctly chained, rebalance-aware cumulative return curve.
    """

    portfolios = sorted(portfolios, key=lambda x: x[0])
    curve = pd.Series(dtype=float)

    # Convert px index to ensure sorted datetime index
    px = px.sort_index()

    for i in range(len(portfolios)):

        # -----------------------------
        # 1. Determine segment dates
        # -----------------------------
        reb_date = portfolios[i][0]

        # Align rebalance date to *actual trading day*
        start_idx = px.index.searchsorted(reb_date)
        if start_idx >= len(px.index):
            continue

        actual_start = px.index[start_idx]

        if i < len(portfolios) - 1:
            next_reb = portfolios[i + 1][0]
        else:
            next_reb = end_date

        # Align next segment end
        end_idx = px.index.searchsorted(next_reb)
        if end_idx >= len(px.index):
            end_idx = len(px.index) - 1

        actual_end = px.index[end_idx]

        # -----------------------------
        # 2. Extract valid price slice
        # -----------------------------
        holdings = portfolios[i][1]

        # Slice prices precisely on the aligned trading dates
        period = px[holdings].loc[actual_start:actual_end]

        # Drop columns with all-NaN
        period = period.dropna(how="all", axis=1)
        if period.empty:
            continue

        # Remove stocks missing price at the rebalance point
        start_row = period.iloc[0]
        valid = start_row[~start_row.isna()].index.tolist()
        if not valid:
            continue

        period = period[valid]

        # 3. Normalize and apply rank-based weights
        # -----------------------------
        norm = period.div(period.iloc[0], axis=1)
        
        # rank-based weights (highest rank = first ticker in `picks`)
        RANK_WEIGHTS = [0.25, 0.25, 0.15, 0.15, 0.10, 0.10]
        #RANK_WEIGHTS = [0.30, 0.30, 0.15, 0.15, 0.1]
        
        num_holdings = len(norm.columns)
        weights = np.array(RANK_WEIGHTS[:num_holdings])
        weights = weights / weights.sum()  # normalize
        
        segment_curve = (norm * weights).sum(axis=1)


        # -----------------------------
        # 4. Chain with previous segment
        # -----------------------------
        if curve.empty:
            curve = segment_curve
        else:
            # Multiply new segment (except first row) by last curve value
            curve = pd.concat([
                curve,
                segment_curve.iloc[1:] * curve.iloc[-1]
            ])

    return curve



# ============================================
# 8) BENCHMARK
# ============================================

def compute_spy_curve(start_date, end):
    spy = yf.download(
        "SPY",
        start=start_date - pd.Timedelta(days=3),
        end=end + pd.Timedelta(days=1),
        auto_adjust=True,
        progress=False
    )["Close"].dropna()

    start_idx = spy.index.searchsorted(start_date)
    spy = spy.iloc[start_idx:]
    spy_curve = spy / spy.iloc[0]
    return spy_curve

def generate_halfyear_dates(start, end):
    """
    Generate 1 April and 1 October boundaries between start and end.
    Returns the FIRST CALENDAR DAY of each half-year (not trading aligned).
    """

    dates = []
    current = pd.Timestamp(start)

    # Normalize start to either 1 April or 1 October
    if current.month < 4:
        current = pd.Timestamp(f"{current.year}-04-01")
    elif current.month < 10:
        current = pd.Timestamp(f"{current.year}-10-01")
    else:
        # If after October, jump to next year's April
        current = pd.Timestamp(f"{current.year+1}-04-01")

    # Generate cycle: Apr → Oct → Apr → Oct → ...
    while current <= end:
        dates.append(current)

        # Move 6 months ahead
        if current.month == 4:
            # go to 1 October same year
            current = pd.Timestamp(f"{current.year}-10-01")
        else:
            # go to 1 April next year
            current = pd.Timestamp(f"{current.year+1}-04-01")

    return dates


def load_sp500_membership_api():
    url = "https://data.nasdaq.com/api/v3/datatables/SHARADAR/SP500"
    params = {
        "api_key": API_KEY
    }
    js = fetch_json_with_retry(url, params)

    cols = [c["name"] for c in js["datatable"]["columns"]]
    df = pd.DataFrame(js["datatable"]["data"], columns=cols)
    df["date"] = pd.to_datetime(df["date"])
    return df


# ============================================
# RUN EVERYTHING
# ============================================

# 1) Load S&P500 membership history + build universe
sharadar_sp500 = load_sp500_membership_api()

# Union of ALL tickers that have EVER been in the S&P500
universe = sharadar_sp500["ticker"].unique().tolist()

# 2) Load price data for the entire union universe
px, buy_dt = load_prices(universe, SNAP, END)

# 3) Download full fundamentals ONCE for entire universe
SF1_FULL = load_sf1_full(universe)

# 4) Generate half year start dates
rebalance_dates = generate_halfyear_dates(SNAP, END)

# 5) Run rebalancing using FULL SF1 with correct membership filtering
portfolios = []

for dt in rebalance_dates:
    
    # Correct S&P500 membership
    members = sp500_members_at_date(sharadar_sp500, dt)

    # Load PIT fundamentals for entire universe
    pit_full = load_fundamentals_pit(SF1_FULL, dt)

    # Restrict to S&P500 members at that date
    pit = pit_full.loc[pit_full.index.intersection(members)]
    
    # ALSO restrict to tickers that have price data (drop delisted)
    valid_price_tickers = set(px.columns)
    pit = pit.loc[pit.index.intersection(valid_price_tickers)]

    # Compute factors + pick stocks
    df_clean = compute_factors(pit)
    picks = select_stocks(df_clean, N_STOCKS)
    portfolios.append((dt, picks))

    tickers_sorted = sorted(picks)
    print(dt.date(), picks)

    # === NEW: print P/L for each holding from this rebalance to the next ===
    print("Performance of each holding:")
    
    for t in picks:
        if t not in px.columns:
            print(f"  {t}: no price data")
            continue
        
        # price at start
        start_idx = px.index.searchsorted(dt)
        start_price = px[t].iloc[start_idx]
    
        # next rebalance or END
        idx = rebalance_dates.index(dt)
        if idx < len(rebalance_dates) - 1:
            next_dt = rebalance_dates[idx + 1]
        else:
            next_dt = END
    
        end_idx = px.index.searchsorted(next_dt)
        end_price = px[t].iloc[end_idx] if end_idx < len(px) else px[t].iloc[-1]
    
        if pd.isna(start_price) or pd.isna(end_price):
            print(f"  {t}: insufficient price data")
            continue
    
        pct = (end_price / start_price - 1) * 100
        print(f"  {t}: {pct:.1f}%")


# 6) Build the performance curve
curve = compute_rebalanced_curve(px, portfolios, END)
spy_curve = compute_spy_curve(portfolios[0][0], END)

# 7) Output final return
final_return = (curve.iloc[-1] - 1) * 100
print("\nRebalanced Portfolio:", f"{final_return:.2f}%")

# 8) Plot
plt.figure(figsize=(12,6))
plt.plot(curve.index, curve.values, label="Rebalanced Portfolio")
plt.plot(spy_curve.index, spy_curve.values, label="SPY")
plt.grid(alpha=0.3)
plt.legend()
plt.title("Rebalanced Portfolio vs SPY")
plt.show()

# ----------------------------------------------------
# ANALYZE WHETHER HIGHER-RANKED PICKS PERFORM BETTER
# ----------------------------------------------------

from collections import defaultdict
import numpy as np

# dict: rank_position -> list of returns
rank_returns = defaultdict(list)

for i, (dt, picks) in enumerate(portfolios):  # portfolios contains (date, [tickers…])
    for rank_pos, t in enumerate(picks):
        if t not in px.columns:
            continue
        
        # Determine start and end dates for this rebalance window
        start_idx = px.index.searchsorted(dt)
        start_price = px[t].iloc[start_idx]
        
        if i < len(portfolios) - 1:
            next_dt = portfolios[i+1][0]
        else:
            next_dt = END
        
        end_idx = px.index.searchsorted(next_dt)
        end_price = px[t].iloc[end_idx] if end_idx < len(px) else px[t].iloc[-1]
        
        if pd.isna(start_price) or pd.isna(end_price):
            continue
        
        pct = (end_price / start_price - 1) * 100
        rank_returns[rank_pos].append(pct)

# Print the results
print("Average return by ranking position:\n")
for pos in sorted(rank_returns.keys()):
    vals = rank_returns[pos]
    avg = np.mean(vals)
    med = np.median(vals)
    print(f"  Rank {pos+1}:  avg {avg:6.2f}%   median {med:6.2f}%   n={len(vals)}")

# Optional: visualize
import matplotlib.pyplot as plt

positions = sorted(rank_returns.keys())
avgs = [np.mean(rank_returns[p]) for p in positions]

plt.figure(figsize=(8,4))
plt.bar([p+1 for p in positions], avgs)
plt.xlabel("Ranking position (1 = highest ranked)")
plt.ylabel("Average return (%)")
plt.title("Do higher-ranked picks perform better?")
plt.grid(alpha=0.3)
plt.show()
