// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go#L5
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
	chan child_DynamicHostDrain50 = [1] of {int}->
	run DynamicHostDrain5(child_DynamicHostDrain50)->
	child_DynamicHostDrain50?0->
stop_process:skip
}

proctype DynamicHostDrain5(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousDynamicHostDrain1161 = [1] of {int}->
	chan child_AnonymousDynamicHostDrain1060 = [1] of {int}->
	Chandef done_ch->
	Chandef retired_ch->
	Chandef update_ch->
	Chandef permit_ch->
	

	if
	:: 1 > 0 -> 
		permit_ch.size = 1->
		run async_monitor(permit_ch)
	:: else -> 
		run sync_monitor(permit_ch)
	fi->
	run sync_monitor(update_ch)->
	run sync_monitor(retired_ch)->
	

	if
	:: 2 > 0 -> 
		done_ch.size = 2->
		run async_monitor(done_ch)
	:: else -> 
		run sync_monitor(done_ch)
	fi->
	run AnonymousDynamicHostDrain106(permit_ch,update_ch,retired_ch,done_ch,child_AnonymousDynamicHostDrain1060)->
	run receiver(child_AnonymousDynamicHostDrain1060)->
	run AnonymousDynamicHostDrain116(permit_ch,update_ch,retired_ch,done_ch,child_AnonymousDynamicHostDrain1161)->
	run receiver(child_AnonymousDynamicHostDrain1161)->
	retired_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:12:2 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:12:2 */->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:13:2 */
	:: done_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:13:2 */->
	:: done_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:13:2 */ -> 
		done_ch.rcving!false
	fi->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:14:2 */
	:: done_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:14:2 */->
	:: done_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:14:2 */ -> 
		done_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousDynamicHostDrain106(Chandef permit_ch;Chandef update_ch;Chandef retired_ch;Chandef done_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	update_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:10:14 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:10:14 */->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:10:29 */
	:: retired_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:10:29 */->
	:: retired_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:10:29 */ -> 
		retired_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:10:40 */
	:: permit_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:10:40 */
	:: permit_ch.sync!false -> 
		permit_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:10:40 */
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:10:53 */
	:: done_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:10:53 */
	:: done_ch.sync!false -> 
		done_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:10:53 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousDynamicHostDrain116(Chandef permit_ch;Chandef update_ch;Chandef retired_ch;Chandef done_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:11:14 */
	:: update_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:11:14 */->
	:: update_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:11:14 */ -> 
		update_ch.rcving!false
	fi->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:11:24 */
	:: permit_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:11:24 */->
	:: permit_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:11:24 */ -> 
		permit_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:11:34 */
	:: done_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:11:34 */
	:: done_ch.sync!false -> 
		done_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/dynamic_host.go:11:34 */
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

