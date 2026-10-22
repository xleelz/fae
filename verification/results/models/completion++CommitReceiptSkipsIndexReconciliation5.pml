// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go#L5
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_CommitReceiptSkipsIndexReconciliation50 = [1] of {int}->
	run CommitReceiptSkipsIndexReconciliation5(child_CommitReceiptSkipsIndexReconciliation50)->
	child_CommitReceiptSkipsIndexReconciliation50?0->
stop_process:skip
}

proctype CommitReceiptSkipsIndexReconciliation5(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousCommitReceiptSkipsIndexReconciliation1182 = [1] of {int}->
	chan child_AnonymousCommitReceiptSkipsIndexReconciliation1061 = [1] of {int}->
	chan child_AnonymousCommitReceiptSkipsIndexReconciliation960 = [1] of {int}->
	Chandef receipt_ch->
	Chandef indexReconciled_ch->
	Chandef branchCommitted_ch->
	run sync_monitor(branchCommitted_ch)->
	run sync_monitor(indexReconciled_ch)->
	run sync_monitor(receipt_ch)->
	run AnonymousCommitReceiptSkipsIndexReconciliation96(branchCommitted_ch,indexReconciled_ch,receipt_ch,child_AnonymousCommitReceiptSkipsIndexReconciliation960)->
	run receiver(child_AnonymousCommitReceiptSkipsIndexReconciliation960)->
	run AnonymousCommitReceiptSkipsIndexReconciliation106(branchCommitted_ch,indexReconciled_ch,receipt_ch,child_AnonymousCommitReceiptSkipsIndexReconciliation1061)->
	run receiver(child_AnonymousCommitReceiptSkipsIndexReconciliation1061)->
	run AnonymousCommitReceiptSkipsIndexReconciliation118(receipt_ch,branchCommitted_ch,indexReconciled_ch,child_AnonymousCommitReceiptSkipsIndexReconciliation1182)->
	run receiver(child_AnonymousCommitReceiptSkipsIndexReconciliation1182)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:12:2 */
	:: receipt_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:12:2 */->
	:: receipt_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:12:2 */ -> 
		receipt_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousCommitReceiptSkipsIndexReconciliation96(Chandef branchCommitted_ch;Chandef indexReconciled_ch;Chandef receipt_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:9:14 */
	:: branchCommitted_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:9:14 */
	:: branchCommitted_ch.sync!false -> 
		branchCommitted_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:9:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousCommitReceiptSkipsIndexReconciliation106(Chandef branchCommitted_ch;Chandef indexReconciled_ch;Chandef receipt_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:10:14 */
	:: branchCommitted_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:10:14 */->
	:: branchCommitted_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:10:14 */ -> 
		branchCommitted_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:10:33 */
	:: indexReconciled_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:10:33 */
	:: indexReconciled_ch.sync!false -> 
		indexReconciled_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:10:33 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousCommitReceiptSkipsIndexReconciliation118(Chandef receipt_ch;Chandef branchCommitted_ch;Chandef indexReconciled_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:11:14 */
	:: receipt_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:11:14 */
	:: receipt_ch.sync!false -> 
		receipt_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/index.go:11:14 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

