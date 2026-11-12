// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go#L5
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
	chan child_Completion50 = [1] of {int}->
	run Completion5(child_Completion50)->
	child_Completion50?0->
stop_process:skip
}

proctype Completion5(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_complete140 = [1] of {int}->
	Chandef timer_ch->
	Chandef cancel_ch->
	Chandef result_ch->
	

	if
	:: 1 > 0 -> 
		result_ch.size = 1->
		run async_monitor(result_ch)
	:: else -> 
		run sync_monitor(result_ch)
	fi->
	

	if
	:: 1 > 0 -> 
		cancel_ch.size = 1->
		run async_monitor(cancel_ch)
	:: else -> 
		run sync_monitor(cancel_ch)
	fi->
	

	if
	:: 1 > 0 -> 
		timer_ch.size = 1->
		run async_monitor(timer_ch)
	:: else -> 
		run sync_monitor(timer_ch)
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:9:2 */
	:: cancel_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:9:2 */
	:: cancel_ch.sync!false -> 
		cancel_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:9:2 */
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:10:2 */
	:: timer_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:10:2 */
	:: timer_ch.sync!false -> 
		timer_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:10:2 */
	fi->
	run complete14(result_ch,cancel_ch,timer_ch,child_complete140)->
	run receiver(child_complete140)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:12:2 */
	:: result_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:12:2 */->
	:: result_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:12:2 */ -> 
		result_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype complete14(Chandef result_ch;Chandef cancel_ch;Chandef timer_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	do /* Select	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:15:2 */
	:: cancel_ch.deq?state,num_msgs -> 
		

		if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:17:3 */
		:: result_ch.enq?ok -> 
			assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:17:3 */
		:: result_ch.sync!false -> 
			result_ch.sending?ok->
			assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:17:3 */
		fi->
		break
	:: cancel_ch.sync?state -> 
		cancel_ch.rcving!false->
		

		if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:17:3 */
		:: result_ch.enq?ok -> 
			assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:17:3 */
		:: result_ch.sync!false -> 
			result_ch.sending?ok->
			assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:17:3 */
		fi->
		break
	:: timer_ch.deq?state,num_msgs -> 
		

		if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:19:3 */
		:: result_ch.enq?ok -> 
			assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:19:3 */
		:: result_ch.sync!false -> 
			result_ch.sending?ok->
			assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:19:3 */
		fi->
		break
	:: timer_ch.sync?state -> 
		timer_ch.rcving!false->
		

		if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:19:3 */
		:: result_ch.enq?ok -> 
			assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:19:3 */
		:: result_ch.sync!false -> 
			result_ch.sending?ok->
			assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:19:3 */
		fi->
		break
	od->
	for10_exit: skip->
	for10_end: skip->
	result_ch.closing?closed /* Close	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:21:2 */->
	assert(!closed) /* Close	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/completion.go:21:2 */->
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

