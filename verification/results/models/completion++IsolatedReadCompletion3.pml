// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go#L3
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
	chan child_IsolatedReadCompletion30 = [1] of {int}->
	run IsolatedReadCompletion3(child_IsolatedReadCompletion30)->
	child_IsolatedReadCompletion30?0->
stop_process:skip
}

proctype IsolatedReadCompletion3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousIsolatedReadCompletion740 = [1] of {int}->
	Chandef done_ch->
	Chandef fast_ch->
	Chandef slow_ch->
	

	if
	:: 1 > 0 -> 
		slow_ch.size = 1->
		run async_monitor(slow_ch)
	:: else -> 
		run sync_monitor(slow_ch)
	fi->
	run sync_monitor(fast_ch)->
	run sync_monitor(done_ch)->
	run AnonymousIsolatedReadCompletion74(slow_ch,fast_ch,done_ch,child_AnonymousIsolatedReadCompletion740)->
	run receiver(child_AnonymousIsolatedReadCompletion740)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:8:2 */
	:: fast_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:8:2 */->
	:: fast_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:8:2 */ -> 
		fast_ch.rcving!false
	fi->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:9:2 */
	:: done_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:9:2 */->
	:: done_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:9:2 */ -> 
		done_ch.rcving!false
	fi->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:10:2 */
	:: slow_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:10:2 */->
	:: slow_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:10:2 */ -> 
		slow_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousIsolatedReadCompletion74(Chandef slow_ch;Chandef fast_ch;Chandef done_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:7:14 */
	:: slow_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:7:14 */
	:: slow_ch.sync!false -> 
		slow_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:7:14 */
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:7:25 */
	:: fast_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:7:25 */
	:: fast_ch.sync!false -> 
		fast_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:7:25 */
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:7:36 */
	:: done_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:7:36 */
	:: done_ch.sync!false -> 
		done_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/readstream.go:7:36 */
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

