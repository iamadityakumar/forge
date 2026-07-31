# Prefix Sums Strategy

Use prefix sums when query ranges [L, R] need to be answered in O(1) time after O(N) preprocessing.

## Formula
P[i] = P[i-1] + A[i]
Sum(L, R) = P[R] - P[L-1]

## When to apply
- Range sum queries without updates.
- Counting subarrays with target sum (use Hash Map + Prefix Sum).