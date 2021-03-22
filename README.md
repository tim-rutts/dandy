# Dandy Project  

Purpose of the project is to learn [golang](https://golang.org) itself as well as **Share Memory By Communicating** pattern. 
The project uses channels, context cancellation, goroutines and graceful shutdown pattern. 
However, right now it works only in *single "thread" mode*.

App downloads **Crocodile** (*Крокодил*) magazine [archive](https://croco.uno) issued in 1922 - 2008 years in USSR (Russia).  


App commandline arguments:

| ARG | Default | Description |
| ----------- | ----------- | ----------- |
| from | n/a | from year. min 1922 (required) |
| to | n/a | to year. max 2008. equals to from year if it is 0 or count is 0 (optional) |
| count | n/a | count of years. used to calc to year if to year is 0 (optional) |
| output | ./download | output folder for downloaded resources (optional) |
| verbose | false | eports progress. disabled by default (optional) |