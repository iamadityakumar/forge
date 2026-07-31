# Sliding Window Strategy

Use sliding window for contiguous subarray or substring problems looking for min/max size matching constraints.

## Algorithm Template
- Expand `right` pointer to include elements into window.
- Shrink `left` pointer when window constraint is violated or optimal condition is reached.