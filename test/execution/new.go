// RUN: llgo -o %t %s
// RUN: %t > %t1 2>&1
// RUN: go run %s > %t2 2>&1
// RUN: diff -u %t1 %t2

package main

func main() {
	x := new(int)
	println(*x)
	*x = 2
	println(*x)
	*x = *x * *x
	println(*x)
}
