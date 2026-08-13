import sys
with open('internal/store/postgres.go', 'r') as f:
    text = f.read()

old = '> $2 - $1::interval'
new = '> $2::timestamptz - $1::interval'

if old in text:
    with open('internal/store/postgres.go', 'w') as f:
        f.write(text.replace(old, new))
    print('replaced successfully')
else:
    print('not found')
